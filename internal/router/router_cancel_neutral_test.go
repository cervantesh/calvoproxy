package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// cancellingRoundTripper fails with the ctx error, the way the real transport
// does when the request context is cancelled mid-flight.
type cancellingRoundTripper struct {
	calls int
	err   error
}

func (c *cancellingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls++
	return nil, c.err
}

// A client that hangs up must not cost the model its score or its circuit. The
// upstream was never given a chance to fail — blaming it for someone else's
// disconnect is the bug fixed for streams in v0.4.0, which never reached the
// non-streaming path.
func TestCancelledRequestDoesNotPenalizeModel(t *testing.T) {
	s := &RouterService{
		Client:        &http.Client{Transport: &cancellingRoundTripper{err: context.Canceled}},
		config:        breakerConfig{FailureThreshold: 3, Cooldown: time.Minute},
		policy:        policyConfig{DefaultProfile: "coding", Profiles: map[string][]string{"coding": {"a/b:free"}}},
		modelBreakers: make(map[string]*modelBreakerState),
	}
	attempt := modelAttempt{Profile: "coding", Model: "a/b:free",
		BreakerPolicy: BreakerPolicy{FailureThreshold: 3, Cooldown: time.Minute, Eligible: true}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone

	err := s.executeAttempt(ctx, httptest.NewRecorder(), []byte(`{}`), "k", attempt)
	if err == nil {
		t.Fatal("a cancelled attempt should still report an error to the caller")
	}
	var attErr *attemptError
	if !errors.As(err, &attErr) {
		t.Fatalf("unexpected error type %T", err)
	}
	if !attErr.SkipModel {
		t.Error("a cancel must be a soft skip, not a verdict on the model")
	}

	key := s.breakerKey(attempt)
	s.breakerMu.RLock()
	st := s.modelBreakers[key]
	s.breakerMu.RUnlock()
	if st != nil && st.ConsecutiveFailures > 0 {
		t.Errorf("client cancel recorded %d breaker failure(s) against the model", st.ConsecutiveFailures)
	}
	// scoreForAttempt, not the raw field: a fresh state stores Score 0 and only
	// becomes meaningful once ScoreUpdatedAt is set, so the raw field cannot
	// distinguish "never scored" from "scored to zero".
	if got := s.scoreForAttempt(attempt, s.scoreEnv()); got != scoreInitial {
		t.Errorf("client cancel moved the model score off neutral: %.3f (want %.3f)", got, scoreInitial)
	}
}

// Our own per-attempt timeout IS evidence the model is too slow, so it must
// still count. Without this the cancel exemption would silently disable the
// timeout signal entirely.
func TestDeadlineExceededStillPenalizes(t *testing.T) {
	s := &RouterService{
		Client:        &http.Client{Transport: &cancellingRoundTripper{err: context.DeadlineExceeded}},
		config:        breakerConfig{FailureThreshold: 3, Cooldown: time.Minute},
		policy:        policyConfig{DefaultProfile: "coding", Profiles: map[string][]string{"coding": {"a/b:free"}}},
		modelBreakers: make(map[string]*modelBreakerState),
	}
	attempt := modelAttempt{Profile: "coding", Model: "a/b:free",
		BreakerPolicy: BreakerPolicy{FailureThreshold: 3, Cooldown: time.Minute, Eligible: true}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond) // let it expire

	_ = s.executeAttempt(ctx, httptest.NewRecorder(), []byte(`{}`), "k", attempt)

	s.breakerMu.RLock()
	st := s.modelBreakers[s.breakerKey(attempt)]
	s.breakerMu.RUnlock()
	if st == nil || st.ConsecutiveFailures == 0 {
		t.Error("a per-attempt timeout is real evidence of slowness and must still count")
	}
}

// The host circuit gates EVERY model on the host. Counting cancels there means
// a handful of impatient clients take out all of openrouter.ai.
func TestHostBreakerIgnoresCancelledRequests(t *testing.T) {
	stub := &cancellingRoundTripper{err: context.Canceled}
	tr := &GlobalBreakerTransport{Base: stub, FailureThreshold: 2, Cooldown: time.Minute}
	req, _ := http.NewRequest(http.MethodGet, "http://example.test/x", nil)

	// Far more cancels than the threshold.
	for i := 0; i < 6; i++ {
		_, _ = tr.RoundTrip(req)
	}
	tr.mu.Lock()
	hb := tr.hosts["example.test"]
	tr.mu.Unlock()
	if hb != nil && hb.failures > 0 {
		t.Errorf("cancels counted %d host failure(s); the host was never given a chance to fail", hb.failures)
	}
	if hb != nil && !hb.openUntil.IsZero() {
		t.Error("cancels opened the host circuit for every model on the host")
	}

	// A genuine transport error still opens it — the exemption must be narrow.
	stub.err = errors.New("connection refused")
	for i := 0; i < 2; i++ {
		_, _ = tr.RoundTrip(req)
	}
	tr.mu.Lock()
	hb = tr.hosts["example.test"]
	tr.mu.Unlock()
	if hb == nil || hb.openUntil.IsZero() {
		t.Error("a real transport failure must still open the host circuit")
	}
}
