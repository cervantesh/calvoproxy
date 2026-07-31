package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestPKCEChallenge(t *testing.T) {
	v := "test-verifier-abc123"
	sum := sha256.Sum256([]byte(v))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got := pkceChallenge(v); got != want {
		t.Fatalf("challenge = %q; want %q", got, want)
	}
	tok, err := randomURLToken(32)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 42 { // 32 bytes base64url ≈ 43 chars
		t.Fatalf("verifier too short: %d", len(tok))
	}
	// Two tokens must differ (randomness).
	tok2, _ := randomURLToken(32)
	if tok == tok2 {
		t.Fatal("randomURLToken produced identical tokens")
	}
}

func TestLoopbackOverrideOr(t *testing.T) {
	const def = "https://openrouter.ai/auth"
	cases := []struct {
		val  string
		want string
	}{
		{"", def},
		{"http://127.0.0.1:9999/auth", "http://127.0.0.1:9999/auth"},
		{"http://localhost:8080/auth", "http://localhost:8080/auth"},
		{"https://evil.example.com/auth", def}, // non-loopback override ignored
		{"://bad", def},
	}
	for _, c := range cases {
		t.Setenv("PROXY_OPENROUTER_AUTH_URL", c.val)
		if got := loopbackOverrideOr("PROXY_OPENROUTER_AUTH_URL", def); got != c.want {
			t.Errorf("override %q → %q; want %q", c.val, got, c.want)
		}
	}
}

func TestExchangeCodeForKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		fmt.Fprint(w, `{"key":"sk-or-v1-abcdefghijklmnopqrstuvwxyz"}`)
	}))
	defer srv.Close()
	t.Setenv("PROXY_OPENROUTER_KEYS_URL", srv.URL) // httptest is 127.0.0.1 → honored

	key, err := exchangeCodeForKey(context.Background(), "the-code", "the-verifier")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if key != "sk-or-v1-abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("got key %q", key)
	}

	// Non-200 → error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer bad.Close()
	t.Setenv("PROXY_OPENROUTER_KEYS_URL", bad.URL)
	if _, err := exchangeCodeForKey(context.Background(), "c", "v"); err == nil {
		t.Fatal("expected error on non-200")
	}

	// Bad key shape → error.
	weird := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"key":"not-a-real-key"}`)
	}))
	defer weird.Close()
	t.Setenv("PROXY_OPENROUTER_KEYS_URL", weird.URL)
	if _, err := exchangeCodeForKey(context.Background(), "c", "v"); err == nil {
		t.Fatal("expected error on bad key shape")
	}
}

func TestValidAuthCode(t *testing.T) {
	if !validAuthCode("abc-123_XYZ.~") {
		t.Error("valid token rejected")
	}
	if validAuthCode("") || validAuthCode("has space") || validAuthCode("bad&param") {
		t.Error("invalid code accepted")
	}
}

// TestCallbackFlow drives the loopback callback server end-to-end (minus the
// browser): a GET with a valid code+state is delivered; error and mismatch fail.
func TestCallbackFlow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, ch, stop, err := startCallbackServer(ctx, "expected-state")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	base := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	resp, err := http.Get(base + "?" + url.Values{"code": {"good-code"}, "state": {"expected-state"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	res := <-ch
	if res.err != nil || res.code != "good-code" {
		t.Fatalf("callback: code=%q err=%v", res.code, res.err)
	}
}

// TestCallbackIgnoresPoisonThenAcceptsRealRedirect guards the single-shot
// delivery against local poisoning: junk callbacks (bad state, malformed code)
// must be ignored — not consume the one delivery — so the genuine redirect still
// completes the login.
func TestCallbackIgnoresPoisonThenAcceptsRealRedirect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, ch, stop, _ := startCallbackServer(ctx, "expected-state")
	defer stop()
	base := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	// Poison 1: wrong state. Poison 2: malformed code. Neither may deliver.
	if r, err := http.Get(base + "?" + url.Values{"code": {"c"}, "state": {"wrong"}}.Encode()); err == nil {
		r.Body.Close()
	}
	if r, err := http.Get(base + "?" + url.Values{"code": {"bad code"}, "state": {"expected-state"}}.Encode()); err == nil {
		r.Body.Close()
	}
	select {
	case res := <-ch:
		t.Fatalf("poison callback consumed the delivery: %+v", res)
	case <-time.After(150 * time.Millisecond):
	}

	// The real redirect still wins.
	if r, err := http.Get(base + "?" + url.Values{"code": {"good-code"}, "state": {"expected-state"}}.Encode()); err == nil {
		r.Body.Close()
	}
	select {
	case res := <-ch:
		if res.err != nil || res.code != "good-code" {
			t.Fatalf("real redirect: code=%q err=%v", res.code, res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("real redirect was not delivered after poison attempts")
	}
}

// A provider-signalled error with a matching state IS a definitive answer.
func TestCallbackDeliversProviderError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, ch, stop, _ := startCallbackServer(ctx, "s")
	defer stop()
	if r, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback?state=s&error=access_denied", port)); err == nil {
		r.Body.Close()
	}
	select {
	case res := <-ch:
		if res.err == nil {
			t.Fatal("expected error on ?error=")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("provider error was not delivered")
	}
}

// An unattributed callback (no state) carrying error= must NOT burn the single
// delivery: any local process could otherwise kill a login in flight. A code with
// no state is still accepted (the provider may not echo state).
func TestCallbackUnattributedErrorIsIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, ch, stop, _ := startCallbackServer(ctx, "expected-state")
	defer stop()
	base := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	// No state at all + error= → ignored.
	if r, err := http.Get(base + "?error=access_denied"); err == nil {
		r.Body.Close()
	}
	select {
	case res := <-ch:
		t.Fatalf("unattributed error= consumed the delivery: %+v", res)
	case <-time.After(150 * time.Millisecond):
	}

	// The genuine redirect still completes the login.
	if r, err := http.Get(base + "?" + url.Values{"code": {"good-code"}, "state": {"expected-state"}}.Encode()); err == nil {
		r.Body.Close()
	}
	select {
	case res := <-ch:
		if res.err != nil || res.code != "good-code" {
			t.Fatalf("real redirect: code=%q err=%v", res.code, res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("real redirect was not delivered")
	}
}

// A provider that does not echo state must still be able to complete a login.
func TestCallbackAcceptsCodeWithoutState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, ch, stop, _ := startCallbackServer(ctx, "expected-state")
	defer stop()
	if r, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback?code=no-state-code", port)); err == nil {
		r.Body.Close()
	}
	select {
	case res := <-ch:
		if res.err != nil || res.code != "no-state-code" {
			t.Fatalf("code without state should still work: code=%q err=%v", res.code, res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("code without state was not delivered (would break providers that don't echo state)")
	}
}
