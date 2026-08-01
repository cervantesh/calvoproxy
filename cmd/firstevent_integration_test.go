package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cervantesh/calvoproxy/internal/router"
)

// TestFirstEventBudget_AdvancesChainAndStillAnswers is the end-to-end proof that
// abandoning a queued model actually falls through to the next one rather than
// surfacing an error to the client.
//
// That distinction is the whole feature. The abandonment error must be marked so
// the fallback executor continues; if it is not, `shouldRetryAttempt` stops the
// chain and a merely-slow model becomes a client-visible failure — strictly worse
// than the slow answer it replaced.
//
// The mock upstream reproduces the real shape of the problem: the first model
// accepts the request, returns 200 with an event-stream content type, and then
// emits only OpenRouter's `: OPENROUTER PROCESSING` keepalive comments while it
// sits queued. Comments are bytes, so a naive "first byte" test would be
// satisfied by them and the feature would never fire in the one case it exists
// for.
func TestFirstEventBudget_AdvancesChainAndStillAnswers(t *testing.T) {
	t.Setenv("PROXY_STREAM_FIRST_BYTE_TIMEOUT", "1")
	t.Setenv("PROXY_SCORING_ENABLED", "false") // keep the chain in policy order
	bindHost = "127.0.0.1"

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		if n == 1 {
			// Queued: keepalives only, well past the 1s budget.
			for i := 0; i < 8; i++ {
				fmt.Fprint(w, ": OPENROUTER PROCESSING\n\n")
				if fl != nil {
					fl.Flush()
				}
				time.Sleep(300 * time.Millisecond)
			}
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"from-the-second-model\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer upstream.Close()
	t.Setenv("PROXY_OPENROUTER_URL", upstream.URL)

	svc := router.NewRouterService()
	defer svc.Close()
	mux := newMux(svc, nil)

	body := `{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/coding/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	start := time.Now()
	mux.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("abandoning the first model must not surface an error: status=%d body=%q", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "from-the-second-model") {
		t.Fatalf("the chain did not advance to the next model; got %q", out)
	}
	if strings.Contains(out, "OPENROUTER PROCESSING") {
		t.Error("keepalive comments from the abandoned attempt leaked to the client")
	}
	if calls.Load() < 2 {
		t.Errorf("upstream called %d time(s); the second model was never tried", calls.Load())
	}
	// Bounded by the budget, not by the 2.4s the queued model would have taken.
	if elapsed > 2*time.Second {
		t.Errorf("took %v — the first-event budget did not cut the queued model", elapsed)
	}
	if got := svc.Counters().StreamFirstEventTimeout; got != 1 {
		t.Errorf("abandonment counter = %d, want 1", got)
	}
}

// The last attempt has nothing to fall back to, so cutting it converts a slow
// success into a fast failure — backwards during exactly the broad slowness
// where an answer matters most.
func TestFirstEventBudget_LastAttemptIsExempt(t *testing.T) {
	t.Setenv("PROXY_STREAM_FIRST_BYTE_TIMEOUT", "1")
	t.Setenv("PROXY_SCORING_ENABLED", "false")
	// A single-model chain: the only attempt is also the last one.
	t.Setenv("PROXY_PROVIDER_PROFILES_JSON", `{"coding":["only/model:free"]}`)
	bindHost = "127.0.0.1"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		// Silent well past the budget, then answers.
		time.Sleep(1600 * time.Millisecond)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"slow-but-real\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer upstream.Close()
	t.Setenv("PROXY_OPENROUTER_URL", upstream.URL)

	svc := router.NewRouterService()
	defer svc.Close()
	mux := newMux(svc, nil)

	body := `{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/coding/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "slow-but-real") {
		t.Fatalf("the last attempt must be allowed to be slow; got %q", rec.Body.String())
	}
	if got := svc.Counters().StreamFirstEventTimeout; got != 0 {
		t.Errorf("the last attempt was cut anyway (counter=%d)", got)
	}
}

// A 400 from one provider is not a verdict on the request. Observed in
// production: a client exposing more than 64 tools got
// `400 invalid_request_error: "at most 64 tools are allowed"` from one
// provider, and because 400 is otherwise terminal the whole chain stopped —
// the user got an error in 0.8s from a request every other model could serve.
func TestUpstream400AdvancesToTheNextModel(t *testing.T) {
	t.Setenv("PROXY_SCORING_ENABLED", "false")
	bindHost = "127.0.0.1"

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":"invalid_request_error","message":"at most 64 tools are allowed","param":"tools"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"served-by-the-next-model"}}]}`)
	}))
	defer upstream.Close()
	t.Setenv("PROXY_OPENROUTER_URL", upstream.URL)

	svc := router.NewRouterService()
	defer svc.Close()
	mux := newMux(svc, nil)

	body := `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/coding/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("one provider's 400 must not end the chain: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "served-by-the-next-model") {
		t.Errorf("the chain did not advance past the 400; got %q", rec.Body.String())
	}
	if calls.Load() < 2 {
		t.Errorf("upstream called %d time(s); the next model was never tried", calls.Load())
	}
}

// A profile name is a request, not a promise: the chain reorders and falls
// through, so "coding" can be served by any member. Without these headers a
// caller cannot tell a first-choice answer from a fallback — which is how a
// design review ended up answered by the third model in the chain while the
// caller believed it had the first.
func TestServedModelHeadersRevealFallback(t *testing.T) {
	t.Setenv("PROXY_SCORING_ENABLED", "false")
	bindHost = "127.0.0.1"

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":"upstream down"}`)
			return
		}
		fmt.Fprint(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upstream.Close()
	t.Setenv("PROXY_OPENROUTER_URL", upstream.URL)

	svc := router.NewRouterService()
	defer svc.Close()
	mux := newMux(svc, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/coding/chat/completions",
		strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Calvoproxy-Profile"); got != "coding" {
		t.Errorf("profile header = %q, want coding", got)
	}
	if got := rec.Header().Get("X-Calvoproxy-Model"); got == "" {
		t.Error("the served model must be named in the response")
	}
	// The first model 5xx'd, so this answer came from a fallback. That is the
	// whole point: an HTTP 200 alone cannot tell the caller that.
	if got := rec.Header().Get("X-Calvoproxy-Attempt"); got != "2" {
		t.Errorf("attempt header = %q, want 2 (served by a fallback)", got)
	}
}
