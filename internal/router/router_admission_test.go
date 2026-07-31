package router

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdmission_DisabledAdmitsImmediately(t *testing.T) {
	a := &admissionControl{} // disabled
	release, ok := a.acquire(context.Background())
	if !ok {
		t.Fatal("disabled admission must admit")
	}
	release() // must be safe
	if a.enabled() {
		t.Fatal("empty admission should report disabled")
	}
}

func TestAdmission_CapsConcurrency(t *testing.T) {
	a := &admissionControl{sem: make(chan struct{}, 2), timeout: 50 * time.Millisecond}

	// Fill both slots.
	r1, ok1 := a.acquire(context.Background())
	r2, ok2 := a.acquire(context.Background())
	if !ok1 || !ok2 {
		t.Fatal("first two acquires should succeed")
	}
	// Third must be rejected after the admission timeout (no slot frees).
	start := time.Now()
	_, ok3 := a.acquire(context.Background())
	if ok3 {
		t.Fatal("third acquire should be rejected while at capacity")
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("rejection should wait for the admission timeout")
	}
	// Releasing a slot lets the next in.
	r1()
	r4, ok4 := a.acquire(context.Background())
	if !ok4 {
		t.Fatal("acquire after release should succeed")
	}
	r2()
	r4()
}

func TestAdmission_ContextCancelRejects(t *testing.T) {
	a := &admissionControl{sem: make(chan struct{}, 1), timeout: time.Hour}
	r1, _ := a.acquire(context.Background())
	defer r1()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := a.acquire(ctx); ok {
		t.Fatal("cancelled context should reject admission")
	}
}

func TestAdmission_ReleasesUnderConcurrency(t *testing.T) {
	a := &admissionControl{sem: make(chan struct{}, 4), timeout: time.Second}
	var inFlight, maxSeen atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, ok := a.acquire(context.Background())
			if !ok {
				return
			}
			n := inFlight.Add(1)
			for {
				m := maxSeen.Load()
				if n <= m || maxSeen.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inFlight.Add(-1)
			release()
		}()
	}
	wg.Wait()
	if maxSeen.Load() > 4 {
		t.Fatalf("concurrency exceeded the cap: saw %d in flight", maxSeen.Load())
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("5"); got != 5*time.Second {
		t.Errorf("seconds: got %v", got)
	}
	if got := parseRetryAfter("  10 "); got != 10*time.Second {
		t.Errorf("trimmed seconds: got %v", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Errorf("empty: got %v", got)
	}
	if got := parseRetryAfter("garbage"); got != 0 {
		t.Errorf("garbage: got %v", got)
	}
	if got := parseRetryAfter("-3"); got != 0 {
		t.Errorf("negative: got %v", got)
	}
}

func TestRecordFailure_RetryAfterExtendsCooldown(t *testing.T) {
	s := &RouterService{
		config:        breakerConfig{FailureThreshold: 1, Cooldown: 2 * time.Second},
		modelBreakers: map[string]*modelBreakerState{},
	}
	attempt := modelAttempt{Profile: "coding", Model: "m1"}
	// One failure trips the circuit (threshold 1); Retry-After 60s must win over
	// the 2s default cooldown.
	s.recordFailure(attempt, 429, "rate limited", 60*time.Second)
	state := s.modelBreakers[s.breakerKey(attempt)]
	remaining := time.Until(state.OpenUntil)
	if remaining < 55*time.Second {
		t.Fatalf("Retry-After should extend cooldown to ~60s, got %v", remaining)
	}

	// A LATER failure with the default (short) cooldown must NOT shorten the
	// long Retry-After window already set.
	openBefore := state.OpenUntil
	s.recordFailure(attempt, 500, "boom") // no Retry-After → default 2s cooldown
	if state.OpenUntil.Before(openBefore) {
		t.Fatalf("a later short-cooldown failure shortened the Retry-After window: %v < %v", state.OpenUntil, openBefore)
	}
}
