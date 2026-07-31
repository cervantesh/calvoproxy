package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cervantesh/calvoproxy/internal/router"
)

// TestStreaming_NotCutByRequestTimeout proves the F1 fix end-to-end: a streamed
// (SSE) response that outlasts PROXY_REQUEST_TIMEOUT_SECONDS is delivered in full
// instead of being cut mid-stream by the old whole-request client timeout.
func TestStreaming_NotCutByRequestTimeout(t *testing.T) {
	// Per-attempt / header timeout of 1s; the stream deliberately runs ~1.5s.
	t.Setenv("PROXY_REQUEST_TIMEOUT_SECONDS", "1")
	bindHost = "127.0.0.1"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "data: chunk%d\n\n", i)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(300 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer upstream.Close()
	t.Setenv("PROXY_OPENROUTER_URL", upstream.URL)

	svc := router.NewRouterService()
	mux := newMux(svc, nil)

	body := `{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/coding/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	start := time.Now()
	mux.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for i := 0; i < 5; i++ {
		if !strings.Contains(out, fmt.Sprintf("chunk%d", i)) {
			t.Fatalf("stream was cut: missing chunk%d — got %q", i, out)
		}
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("stream did not complete ([DONE] missing): %q", out)
	}
	// The whole thing must have run past the 1s per-attempt/header timeout,
	// proving the stream was not capped by it.
	if elapsed < 1200*time.Millisecond {
		t.Fatalf("stream finished too fast (%v) — was it cut?", elapsed)
	}
}
