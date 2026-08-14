package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cervantesh/calvoproxy/internal/router"
)

func TestResolveAPIKey_PrefersAuthorizationThenEnv(t *testing.T) {
	// On a loopback bind the env key fallback applies (unchanged behaviour); the
	// public-bind gate is exercised separately in security_test.go.
	bindHost = "127.0.0.1"
	t.Setenv("OPENROUTER_API_KEY", "env-key")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if got := resolveAPIKey(req); got != "env-key" {
		t.Fatalf("expected env key fallback, got %q", got)
	}

	req.Header.Set("Authorization", "Bearer header-key")
	if got := resolveAPIKey(req); got != "header-key" {
		t.Fatalf("expected header key override, got %q", got)
	}
}

// TestResolveAPIKey_SchemeOnlyHeaderFallsBackToAmbient covers the failure a
// desktop client hit in the field: with its API-key setting left blank it sent
// `Authorization: Bearer` (scheme, no token). That does NOT extract to "" —
// Go trims the trailing space, so the bearer-prefix test fails and the literal
// word "Bearer" comes back as the key. Being non-empty, it skipped the ambient
// fallback and was forwarded upstream, which answered 401 "Missing
// Authentication header" even though a valid stored credential existed.
func TestResolveAPIKey_SchemeOnlyHeaderFallsBackToAmbient(t *testing.T) {
	bindHost = "127.0.0.1"
	t.Setenv("OPENROUTER_API_KEY", "env-key")

	for _, header := range []string{"Bearer ", "Bearer", "bearer", "  Bearer  ", "dummy", ""} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		if got := resolveAPIKey(req); got != "env-key" {
			t.Fatalf("Authorization %q carries no usable key and must fall back to the ambient one, got %q", header, got)
		}
	}

	// A real token must still win over the ambient key — the fallback widening
	// must not start swallowing credentials that merely contain the word.
	for _, tc := range []struct{ header, want string }{
		{"Bearer sk-or-v1-real", "sk-or-v1-real"},
		{"Bearer bearer-shaped-token", "bearer-shaped-token"},
		{"sk-or-v1-no-scheme", "sk-or-v1-no-scheme"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("Authorization", tc.header)
		if got := resolveAPIKey(req); got != tc.want {
			t.Fatalf("Authorization %q should resolve to %q, got %q", tc.header, tc.want, got)
		}
	}
}

func TestNewMux_RejectsUnauthorizedAndServesHealth(t *testing.T) {
	// Deterministic 401: no env key to fall back to (the host running the suite
	// may have OPENROUTER_API_KEY set, and a prior test may have left bindHost on
	// loopback where the env-key fallback applies).
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("CEREBRAS_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
	t.Setenv("PROXY_ADMIN_TOKEN", "")
	mux := newMux(router.NewRouterService(), nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized, got %d", rec.Code)
	}

	healthReq := httptest.NewRequest(http.MethodGet, "/health", nil)
	healthRec := httptest.NewRecorder()
	mux.ServeHTTP(healthRec, healthReq)
	if healthRec.Code != http.StatusOK {
		t.Fatalf("expected health 200, got %d", healthRec.Code)
	}
	var health map[string]any
	if err := json.Unmarshal(healthRec.Body.Bytes(), &health); err != nil {
		t.Fatalf("invalid health JSON: %v", err)
	}
	if _, ok := health["model_policy"].(map[string]any); !ok {
		t.Fatalf("expected model_policy in health response: %+v", health)
	}

	modelPolicyReq := httptest.NewRequest(http.MethodGet, "/health/model-policy", nil)
	modelPolicyRec := httptest.NewRecorder()
	mux.ServeHTTP(modelPolicyRec, modelPolicyReq)
	if modelPolicyRec.Code != http.StatusOK {
		t.Fatalf("expected model policy health 200, got %d", modelPolicyRec.Code)
	}
	var modelPolicy map[string]any
	if err := json.Unmarshal(modelPolicyRec.Body.Bytes(), &modelPolicy); err != nil {
		t.Fatalf("invalid model policy health JSON: %v", err)
	}
	if modelPolicy["default_profile"] == "" {
		t.Fatalf("expected default profile in model policy health: %+v", modelPolicy)
	}

	readyReq := httptest.NewRequest(http.MethodGet, "/ready", nil)
	readyRec := httptest.NewRecorder()
	mux.ServeHTTP(readyRec, readyReq)
	if readyRec.Code != http.StatusOK {
		t.Fatalf("expected ready 200 for fresh router, got %d", readyRec.Code)
	}
}

func TestNewMux_RejectsWrongMethodAndUnknownDynamicRoute(t *testing.T) {
	mux := newMux(router.NewRouterService(), nil)

	req := httptest.NewRequest(http.MethodGet, "/messages", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected method not allowed, got %d", rec.Code)
	}

	notFoundReq := httptest.NewRequest(http.MethodPost, "/v1/unknown/path", nil)
	notFoundRec := httptest.NewRecorder()
	mux.ServeHTTP(notFoundRec, notFoundReq)
	if notFoundRec.Code != http.StatusNotFound {
		t.Fatalf("expected not found for unknown dynamic route, got %d", notFoundRec.Code)
	}
}
