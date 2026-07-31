package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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
	cbURL, ch, stop, err := startCallbackServer(ctx, "expected-state")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	base := cbURL

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
	cbURL, ch, stop, _ := startCallbackServer(ctx, "expected-state")
	defer stop()
	base := cbURL

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
	cbURL, ch, stop, _ := startCallbackServer(ctx, "s")
	defer stop()
	if r, err := http.Get(cbURL + "?state=s&error=access_denied"); err == nil {
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
// delivery: any local process could otherwise kill a login in flight. With the
// strict check opted out, a code with no state is still accepted.
func TestCallbackUnattributedErrorIsIgnored(t *testing.T) {
	t.Setenv("PROXY_OAUTH_REQUIRE_STATE", "false")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cbURL, ch, stop, _ := startCallbackServer(ctx, "expected-state")
	defer stop()
	base := cbURL

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

// A provider that does not echo state must still be able to complete a login —
// but only when the operator opts out of the (default-on) strict check.
func TestCallbackAcceptsCodeWithoutState(t *testing.T) {
	t.Setenv("PROXY_OAUTH_REQUIRE_STATE", "false")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cbURL, ch, stop, _ := startCallbackServer(ctx, "expected-state")
	defer stop()
	if r, err := http.Get(cbURL + "?code=no-state-code"); err == nil {
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

// TestCallbackSecretPath is the core of the issue-#7 hardening: the callback
// lives on an unguessable path, so a local process that doesn't know it cannot
// reach the handler at all — it can neither inject its own authorization code nor
// burn the single-shot delivery. This protects the flow even when the provider
// does not echo the state parameter.
func TestCallbackSecretPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cbURL, ch, stop, err := startCallbackServer(ctx, "expected-state")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	u, err := url.Parse(cbURL)
	if err != nil {
		t.Fatal(err)
	}
	// The path must carry a high-entropy secret, not be a guessable constant.
	if !strings.HasPrefix(u.Path, "/callback/") || len(strings.TrimPrefix(u.Path, "/callback/")) < 42 {
		t.Fatalf("callback path should carry a 32-byte secret, got %q", u.Path)
	}
	origin := "http://" + u.Host

	// Every guessable path an attacker would try must 404 and never deliver.
	for _, p := range []string{"/callback", "/callback/", "/callback/guessed", "/", "/callback/" + strings.Repeat("a", 43)} {
		resp, err := http.Get(origin + p + "?" + url.Values{"code": {"attacker-code"}, "state": {"expected-state"}}.Encode())
		if err != nil {
			continue
		}
		body := resp.StatusCode
		resp.Body.Close()
		if body != http.StatusNotFound {
			t.Errorf("path %q should 404, got %d", p, body)
		}
	}
	select {
	case res := <-ch:
		t.Fatalf("a request to a wrong path reached the handler: %+v", res)
	case <-time.After(150 * time.Millisecond):
	}

	// The real (secret) path still completes the login.
	if r, err := http.Get(cbURL + "?" + url.Values{"code": {"good-code"}, "state": {"expected-state"}}.Encode()); err == nil {
		r.Body.Close()
	}
	select {
	case res := <-ch:
		if res.err != nil || res.code != "good-code" {
			t.Fatalf("secret path should deliver: code=%q err=%v", res.code, res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the genuine redirect on the secret path was not delivered")
	}
}

// Strict mode IGNORES a callback with no matching state — it must not deliver a
// refusal, because that would let anyone who merely learned the callback path
// burn the one-shot and kill the login without knowing the state.
func TestCallbackRequireStateStrictMode(t *testing.T) {
	t.Setenv("PROXY_OAUTH_REQUIRE_STATE", "true")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cbURL, ch, stop, _ := startCallbackServer(ctx, "expected-state")
	defer stop()

	if r, err := http.Get(cbURL + "?code=no-state-code"); err == nil {
		r.Body.Close()
	}
	select {
	case res := <-ch:
		t.Fatalf("a state-less callback must be ignored, not consume the delivery: %+v", res)
	case <-time.After(200 * time.Millisecond):
	}
}

// With strict mode on, a properly attributed callback still works.
func TestCallbackRequireStateAcceptsMatching(t *testing.T) {
	t.Setenv("PROXY_OAUTH_REQUIRE_STATE", "true")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cbURL, ch, stop, _ := startCallbackServer(ctx, "expected-state")
	defer stop()

	if r, err := http.Get(cbURL + "?" + url.Values{"code": {"good-code"}, "state": {"expected-state"}}.Encode()); err == nil {
		r.Body.Close()
	}
	select {
	case res := <-ch:
		if res.err != nil || res.code != "good-code" {
			t.Fatalf("strict mode should accept a matching state: code=%q err=%v", res.code, res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("matching state was not delivered under strict mode")
	}
}

// A concurrent flood of wrong-path requests must not consume the one-shot
// delivery, and the genuine redirect must still win afterwards.
func TestCallbackWrongPathFloodThenRealRedirect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cbURL, ch, stop, _ := startCallbackServer(ctx, "expected-state")
	defer stop()
	u, _ := url.Parse(cbURL)
	origin := "http://" + u.Host

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := fmt.Sprintf("%s/callback/guess-%d?code=attacker-code&state=expected-state", origin, i)
			if r, err := http.Get(p); err == nil {
				r.Body.Close()
			}
		}(i)
	}
	wg.Wait()
	select {
	case res := <-ch:
		t.Fatalf("a wrong-path flood consumed the delivery: %+v", res)
	case <-time.After(150 * time.Millisecond):
	}

	if r, err := http.Get(cbURL + "?" + url.Values{"code": {"good-code"}, "state": {"expected-state"}}.Encode()); err == nil {
		r.Body.Close()
	}
	select {
	case res := <-ch:
		if res.err != nil || res.code != "good-code" {
			t.Fatalf("real redirect after flood: code=%q err=%v", res.code, res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("real redirect was not delivered after a wrong-path flood")
	}
}

// A deeper path under the secret must not match either (exact registration).
func TestCallbackSubPathDoesNotDeliver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cbURL, ch, stop, _ := startCallbackServer(ctx, "s")
	defer stop()
	if r, err := http.Get(cbURL + "/extra?code=c&state=s"); err == nil {
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("sub-path should 404, got %d", r.StatusCode)
		}
		r.Body.Close()
	}
	select {
	case res := <-ch:
		t.Fatalf("a sub-path reached the handler: %+v", res)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestRequireStateEnvParsing(t *testing.T) {
	// Default is TRUE (OpenRouter echoes state); only an explicit falsey value
	// opts out.
	cases := map[string]bool{"": true, "1": true, "true": true, "TRUE": true,
		"yes": true, "on": true, "garbage": true,
		"0": false, "false": false, "FALSE": false, "no": false, "off": false}
	for val, want := range cases {
		t.Setenv("PROXY_OAUTH_REQUIRE_STATE", val)
		if got := requireState(); got != want {
			t.Errorf("PROXY_OAUTH_REQUIRE_STATE=%q → %v; want %v", val, got, want)
		}
	}
}

// Each login must get a fresh, unguessable callback path.
func TestCallbackPathIsFreshPerLogin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, _, stopA, _ := startCallbackServer(ctx, "s")
	defer stopA()
	b, _, stopB, _ := startCallbackServer(ctx, "s")
	defer stopB()
	pa, _ := url.Parse(a)
	pb, _ := url.Parse(b)
	if pa.Path == pb.Path {
		t.Fatalf("callback path must be unique per login, got %q twice", pa.Path)
	}
	if pa.Path == "/callback" || pb.Path == "/callback" {
		t.Fatal("callback path must not be the guessable constant /callback")
	}
}

// Strict state is ON by default (verified: OpenRouter echoes state), so a
// callback without a matching state is refused unless explicitly opted out.
func TestCallbackRequireStateDefaultsOn(t *testing.T) {
	if !requireState() {
		t.Fatal("PROXY_OAUTH_REQUIRE_STATE must default to true")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cbURL, ch, stop, _ := startCallbackServer(ctx, "expected-state")
	defer stop()
	// Junk with no state must be IGNORED (not consume the one-shot)...
	if r, err := http.Get(cbURL + "?code=attacker-code"); err == nil {
		r.Body.Close()
	}
	select {
	case res := <-ch:
		t.Fatalf("a state-less callback burned the login: %+v", res)
	case <-time.After(200 * time.Millisecond):
	}
	// ...and the genuine, properly attributed redirect must still win.
	if r, err := http.Get(cbURL + "?" + url.Values{"code": {"good-code"}, "state": {"expected-state"}}.Encode()); err == nil {
		r.Body.Close()
	}
	select {
	case res := <-ch:
		if res.err != nil || res.code != "good-code" {
			t.Fatalf("real redirect after state-less junk: code=%q err=%v", res.code, res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the genuine redirect was not delivered after state-less junk")
	}
}
