package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"localhost", true},
		{"0.0.0.0", false},
		{"::", false},
		{"", false},
		{"192.168.1.5", false},
		{"10.0.0.1", false},
	}
	for _, c := range cases {
		if got := isLoopbackHost(c.host); got != c.want {
			t.Errorf("isLoopbackHost(%q) = %v; want %v", c.host, got, c.want)
		}
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !constantTimeEqual("secret", "secret") {
		t.Error("equal tokens should compare equal")
	}
	if constantTimeEqual("secret", "secreta") {
		t.Error("different-length tokens must not be equal")
	}
	if constantTimeEqual("secret", "SECRET") {
		t.Error("case-different tokens must not be equal")
	}
	if constantTimeEqual("", "x") {
		t.Error("empty vs non-empty must not be equal")
	}
}

func TestResolveAPIKey_PublicBindRefusesEnvKeyWithoutOptIn(t *testing.T) {
	oldBind := bindHost
	defer func() { bindHost = oldBind }()
	t.Setenv("OPENROUTER_API_KEY", "env-key")

	// Public bind, no opt-in → env key refused for a keyless request.
	bindHost = "0.0.0.0"
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if got := resolveAPIKey(req); got != "" {
		t.Errorf("public bind should refuse env key, got %q", got)
	}

	// Opt-in restores the fallback.
	t.Setenv("PROXY_ALLOW_ENV_KEY_PUBLIC", "true")
	if got := resolveAPIKey(req); got != "env-key" {
		t.Errorf("with opt-in, env key should be used, got %q", got)
	}

	// A real header key is always honoured regardless of bind.
	req.Header.Set("Authorization", "Bearer header-key")
	if got := resolveAPIKey(req); got != "header-key" {
		t.Errorf("header key should win, got %q", got)
	}
}

func TestMetricsAuth_SeparateToken(t *testing.T) {
	handler := metricsAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	call := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code
	}

	// No tokens set → open (follows admin gate, which is open).
	if code := call(""); code != http.StatusOK {
		t.Errorf("open metrics: got %d", code)
	}

	// Metrics token set → only the metrics (or admin) token is accepted.
	t.Setenv("PROXY_METRICS_TOKEN", "mtok")
	t.Setenv("PROXY_ADMIN_TOKEN", "atok")
	if code := call("mtok"); code != http.StatusOK {
		t.Errorf("metrics token should be accepted: got %d", code)
	}
	if code := call("atok"); code != http.StatusOK {
		t.Errorf("admin token should also be accepted for metrics: got %d", code)
	}
	if code := call("wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong token must be rejected: got %d", code)
	}
}

func TestAdmin_ConstantTimeGate(t *testing.T) {
	handler := admin(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	t.Setenv("PROXY_ADMIN_TOKEN", "atok")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing token should be 401, got %d", rec.Code)
	}

	req.Header.Set("X-Admin-Token", "atok")
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid X-Admin-Token should pass, got %d", rec.Code)
	}
}
