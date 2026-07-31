package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cervantesh/calvoproxy/internal/router"
)

// TestMessages_RoutedThroughChainWithFallback proves the F#2 change: an
// Anthropic /messages request now runs through the model chain — a failing first
// model falls over to the next — and hits the upstream /messages endpoint, with
// the response passed through untransformed.
func TestMessages_RoutedThroughChainWithFallback(t *testing.T) {
	bindHost = "127.0.0.1"
	t.Setenv("OPENROUTER_API_KEY", "test-key")

	var calls atomic.Int64
	var sawMessagesPath atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "messages") {
			sawMessagesPath.Store(true)
		}
		_, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		// First attempt fails (rate limited) → the chain must advance; the next
		// attempt succeeds. Proves multi-model fallback on /messages.
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Distinctive Anthropic-shaped body to confirm no chat transform is applied.
		_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"ANTHROPIC_OK"}]}`))
	}))
	defer upstream.Close()
	// dispatchChain derives the messages URL from PROXY_OPENROUTER_URL by swapping
	// /chat/completions → /messages, so point chat at the mock and messages follow.
	t.Setenv("PROXY_OPENROUTER_URL", upstream.URL+"/v1/chat/completions")

	mux := newMux(router.NewRouterService(), nil)
	body := `{"model":"auto","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dummy")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after fallback, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ANTHROPIC_OK") {
		t.Fatalf("expected untransformed Anthropic body, got %q", rec.Body.String())
	}
	if !sawMessagesPath.Load() {
		t.Fatal("upstream was not hit on the /messages path")
	}
	if calls.Load() < 2 {
		t.Fatalf("expected a fallback (>=2 upstream calls), got %d", calls.Load())
	}
}
