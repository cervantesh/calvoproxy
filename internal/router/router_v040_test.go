package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- P3: stream outcomes drive honest scoring ---

// eofReader delivers chunks then a clean EOF.
type eofReader struct {
	chunks [][]byte
	i      int
	closed atomic.Bool
}

func (e *eofReader) Read(p []byte) (int, error) {
	if e.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if e.i >= len(e.chunks) {
		return 0, io.EOF
	}
	n := copy(p, e.chunks[e.i])
	e.i++
	return n, nil
}
func (e *eofReader) Close() error { e.closed.Store(true); return nil }

func TestStreamCopy_OutcomeCompleted(t *testing.T) {
	body := &eofReader{chunks: [][]byte{[]byte("a"), []byte("b")}}
	rec := httptest.NewRecorder()
	if got := streamCopy(context.Background(), rec, body, time.Second, 0); got != streamCompleted {
		t.Fatalf("clean EOF should be streamCompleted, got %v", got)
	}
	if rec.Body.String() != "ab" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// errReader delivers one chunk then a non-EOF error (upstream died).
type errReader struct {
	sent   bool
	closed atomic.Bool
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if !e.sent {
		e.sent = true
		return copy(p, []byte("x")), nil
	}
	return 0, io.ErrUnexpectedEOF
}
func (e *errReader) Close() error { e.closed.Store(true); return nil }

func TestStreamCopy_OutcomeUpstreamError(t *testing.T) {
	rec := httptest.NewRecorder()
	got := streamCopy(context.Background(), rec, &errReader{}, time.Second, 0)
	if got != streamUpstreamError {
		t.Fatalf("mid-stream error should be streamUpstreamError, got %v", got)
	}
	if !got.upstreamFault() {
		t.Fatal("upstream error must count as an upstream fault")
	}
}

// blockingBody never returns data until closed.
type blockingBody struct {
	ch     chan struct{}
	closed atomic.Bool
}

func newBlockingBody() *blockingBody { return &blockingBody{ch: make(chan struct{})} }
func (b *blockingBody) Read(p []byte) (int, error) {
	<-b.ch
	return 0, io.ErrClosedPipe
}
func (b *blockingBody) Close() error {
	if b.closed.CompareAndSwap(false, true) {
		close(b.ch)
	}
	return nil
}

func TestStreamCopy_OutcomeStalledAndClientGone(t *testing.T) {
	// Idle timeout → stalled (an upstream fault).
	rec := httptest.NewRecorder()
	got := streamCopy(context.Background(), rec, newBlockingBody(), 40*time.Millisecond, 0)
	if got != streamStalled || !got.upstreamFault() {
		t.Fatalf("idle stall should be streamStalled (upstream fault), got %v", got)
	}

	// Context cancel → client gone, NOT an upstream fault.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got = streamCopy(ctx, httptest.NewRecorder(), newBlockingBody(), time.Hour, 0)
	if got != streamClientGone {
		t.Fatalf("cancelled ctx should be streamClientGone, got %v", got)
	}
	if got.upstreamFault() {
		t.Fatal("client disconnect must NOT be blamed on the model")
	}

	// Max-duration backstop → not an upstream fault either (our own policy).
	got = streamCopy(context.Background(), httptest.NewRecorder(), newBlockingBody(), time.Hour, 40*time.Millisecond)
	if got != streamMaxReached || got.upstreamFault() {
		t.Fatalf("max duration should be streamMaxReached and not a fault, got %v", got)
	}
}

// TestResolveProbe_ReleasesCircuitWithoutScoring is the core of the P3 fix: a
// streaming attempt must free the half-open probe at header time (so recovery
// single-flight doesn't wedge for the whole stream) WITHOUT counting a success.
func TestResolveProbe_ReleasesCircuitWithoutScoring(t *testing.T) {
	s := &RouterService{
		config:        breakerConfig{RequestTimeout: 45 * time.Second, FailureThreshold: 3},
		modelBreakers: map[string]*modelBreakerState{},
	}
	attempt := modelAttempt{Profile: "p", Model: "m"}
	key := s.breakerKey(attempt)
	s.modelBreakers[key] = &modelBreakerState{
		OpenUntil: time.Now().Add(-time.Second), // half-open
		Score:     0.5,
	}
	if !s.tryStartAttempt(attempt) {
		t.Fatal("probe should be claimable")
	}
	scoreBefore := s.modelBreakers[key].Score

	s.resolveProbe(attempt)

	st := s.modelBreakers[key]
	if !st.ProbeUntil.IsZero() || !st.OpenUntil.IsZero() || st.ConsecutiveFailures != 0 {
		t.Fatalf("resolveProbe must clear probe/open/failures, got %+v", st)
	}
	if st.Score != scoreBefore {
		t.Fatalf("resolveProbe must NOT change the score (%v → %v)", scoreBefore, st.Score)
	}
	// And the circuit is usable again immediately.
	if !s.tryStartAttempt(attempt) {
		t.Fatal("after resolveProbe the model should be attemptable")
	}
}

// --- P4: host breaker treats 429 as neutral, and single-flights recovery ---

type stubRoundTripper struct {
	mu     sync.Mutex
	status int
	calls  int
}

func (s *stubRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return &http.Response{StatusCode: s.status, Body: io.NopCloser(nil), Header: http.Header{}}, nil
}

func newHostBreaker(status int) (*GlobalBreakerTransport, *stubRoundTripper) {
	stub := &stubRoundTripper{status: status}
	return &GlobalBreakerTransport{Base: stub, FailureThreshold: 2, Cooldown: 50 * time.Millisecond}, stub
}

func TestHostBreaker_429IsNeutral(t *testing.T) {
	tr, _ := newHostBreaker(http.StatusTooManyRequests)
	req, _ := http.NewRequest(http.MethodGet, "http://example.test/x", nil)

	// Accumulate one real fault first.
	tr.hosts = map[string]*hostBreakerState{"example.test": {failures: 1}}

	// A wall of 429s must neither open the circuit nor erase the existing failure.
	for i := 0; i < 5; i++ {
		if _, err := tr.RoundTrip(req); err != nil {
			t.Fatalf("429 must not trip the host circuit: %v", err)
		}
	}
	if got := tr.hosts["example.test"].failures; got != 1 {
		t.Fatalf("429 should be neutral: failures = %d, want 1", got)
	}
	if !tr.hosts["example.test"].openUntil.IsZero() {
		t.Fatal("429 must not open the host circuit (it would blackhole every model on the host)")
	}
}

func TestHostBreaker_OpensOn5xxAndSingleFlightsRecovery(t *testing.T) {
	tr, stub := newHostBreaker(http.StatusServiceUnavailable)
	req, _ := http.NewRequest(http.MethodGet, "http://example.test/x", nil)

	// Two 503s (threshold=2) open the circuit.
	tr.RoundTrip(req)
	tr.RoundTrip(req)
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("host circuit should be open after the failure threshold")
	}

	// After the cooldown, exactly ONE probe may go through.
	time.Sleep(60 * time.Millisecond)
	stub.mu.Lock()
	before := stub.calls
	stub.mu.Unlock()

	var passed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tr.RoundTrip(req); err == nil {
				passed.Add(1)
			}
		}()
	}
	wg.Wait()
	stub.mu.Lock()
	after := stub.calls
	stub.mu.Unlock()
	if after-before != 1 {
		t.Fatalf("half-open should let exactly one probe reach the host, got %d", after-before)
	}
	if passed.Load() != 1 {
		t.Fatalf("exactly one caller should pass the half-open gate, got %d", passed.Load())
	}
}

// --- P7: chain-exhaustion Retry-After ---

func TestRetryAfterForAttempts(t *testing.T) {
	s := &RouterService{modelBreakers: map[string]*modelBreakerState{}}
	a1 := modelAttempt{Profile: "p", Model: "m1"}
	a2 := modelAttempt{Profile: "p", Model: "m2"}
	s.modelBreakers[s.breakerKey(a1)] = &modelBreakerState{OpenUntil: time.Now().Add(30 * time.Second)}
	s.modelBreakers[s.breakerKey(a2)] = &modelBreakerState{OpenUntil: time.Now().Add(5 * time.Second)}

	got := s.retryAfterForAttempts([]modelAttempt{a1, a2})
	if got < 4*time.Second || got > 6*time.Second {
		t.Fatalf("should report the SOONEST cooldown (~5s), got %v", got)
	}

	// Nothing open → no hint.
	s2 := &RouterService{modelBreakers: map[string]*modelBreakerState{}}
	if got := s2.retryAfterForAttempts([]modelAttempt{a1}); got != 0 {
		t.Fatalf("no open circuits → 0, got %v", got)
	}
}

// --- P10: router counters ---

func TestRouterCounters(t *testing.T) {
	s := &RouterService{}
	s.recordStreamOutcome(streamCompleted)
	s.recordStreamOutcome(streamStalled)
	s.recordStreamOutcome(streamStalled)
	s.recordStreamOutcome(streamClientGone)
	s.counters.admissionRejected.Add(3)
	s.counters.capabilityRefused.Add(2)

	c := s.Counters()
	if c.StreamsCompleted != 1 || c.StreamsStalled != 2 || c.StreamsClientGone != 1 {
		t.Fatalf("stream counters wrong: %+v", c)
	}
	if c.AdmissionRejected != 3 || c.CapabilityRefused != 2 {
		t.Fatalf("event counters wrong: %+v", c)
	}
}

// --- P8: cheap health facts agree with the full snapshot ---

func TestHealthFactsMatchesHealth(t *testing.T) {
	s := NewRouterService()
	defer s.Close()
	full := s.Health()
	facts := s.healthFacts()
	if facts.Ready != full.Ready || facts.Status != full.Status || facts.OpenCircuitCount != full.OpenCircuitCount {
		t.Fatalf("healthFacts %+v disagrees with Health %v/%v/%d",
			facts, full.Ready, full.Status, full.OpenCircuitCount)
	}
}

// cancelAwareReader errors like a torn-down connection — what really happens when
// the client cancels: the upstream read fails at the same moment the ctx dies.
type cancelAwareReader struct{ closed atomic.Bool }

func (c *cancelAwareReader) Read(p []byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (c *cancelAwareReader) Close() error               { c.closed.Store(true); return nil }

// A read error that coincides with a cancelled context is the CLIENT leaving, not
// the model failing — blaming the model would open its circuit when a user hits
// Ctrl-C mid-stream.
func TestStreamCopy_CancelledReadErrorIsClientGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := streamCopy(ctx, httptest.NewRecorder(), &cancelAwareReader{}, time.Hour, 0)
	if got != streamClientGone {
		t.Fatalf("read error under a cancelled ctx should be streamClientGone, got %v", got)
	}
	if got.upstreamFault() {
		t.Fatal("a client disconnect must never be scored against the model")
	}
}

// The same error with a LIVE context is a genuine upstream fault.
func TestStreamCopy_LiveReadErrorIsUpstreamFault(t *testing.T) {
	got := streamCopy(context.Background(), httptest.NewRecorder(), &cancelAwareReader{}, time.Hour, 0)
	if got != streamUpstreamError || !got.upstreamFault() {
		t.Fatalf("read error with a live ctx should be an upstream fault, got %v", got)
	}
}
