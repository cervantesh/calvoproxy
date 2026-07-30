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

func TestNewMux_RejectsUnauthorizedAndServesHealth(t *testing.T) {
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
