package main

import (
	"testing"
	"time"
)

func TestIdleTrackerMarkResetsIdle(t *testing.T) {
	tr := newIdleTracker()
	tr.last.Store(time.Now().Add(-time.Hour).UnixNano())
	if tr.idleFor() < 30*time.Minute {
		t.Fatalf("expected large idleFor before mark, got %s", tr.idleFor())
	}
	tr.mark()
	if tr.idleFor() > time.Minute {
		t.Fatalf("mark should reset idleFor, got %s", tr.idleFor())
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
