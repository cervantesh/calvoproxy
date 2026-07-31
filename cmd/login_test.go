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

func TestCallbackStateMismatchAndError(t *testing.T) {
	// State mismatch.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, ch, stop, _ := startCallbackServer(ctx, "expected-state")
	defer stop()
	base := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	http.Get(base + "?" + url.Values{"code": {"c"}, "state": {"wrong"}}.Encode())
	if res := <-ch; res.err == nil {
		t.Fatal("expected state-mismatch error")
	}

	// error= param.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	port2, ch2, stop2, _ := startCallbackServer(ctx2, "s")
	defer stop2()
	http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback?error=access_denied", port2))
	if res := <-ch2; res.err == nil {
		t.Fatal("expected error on ?error=")
	}
}
