package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIdleTrackerIgnoresProbes(t *testing.T) {
	tr := newIdleTracker(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	tr.last.Store(time.Now().Add(-time.Hour).UnixNano())
	old := tr.last.Load()

	// A probe must NOT reset activity.
	tr.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	if tr.last.Load() != old {
		t.Fatal("/health should not count as activity")
	}

	// A real request must reset activity.
	tr.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if tr.last.Load() == old {
		t.Fatal("real request should reset activity")
	}
	if tr.idleFor() > time.Minute {
		t.Fatalf("idleFor should be small after activity, got %s", tr.idleFor())
	}
}

func TestIdleTimeoutFromEnv(t *testing.T) {
	t.Setenv("PROXY_IDLE_TIMEOUT", "15m")
	if got := idleTimeoutFromEnv(); got != 15*time.Minute {
		t.Fatalf("expected 15m, got %s", got)
	}
	t.Setenv("PROXY_IDLE_TIMEOUT", "")
	if got := idleTimeoutFromEnv(); got != 0 {
		t.Fatalf("expected 0 when unset, got %s", got)
	}
	t.Setenv("PROXY_IDLE_TIMEOUT", "garbage")
	if got := idleTimeoutFromEnv(); got != 0 {
		t.Fatalf("expected 0 on invalid, got %s", got)
	}
}
