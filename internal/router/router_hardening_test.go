package router

import (
	"context"
	"io"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- F1: streamCopy idle timeout + live-stream completion ---

// slowReader emits `chunks` byte-slices, each after `gap`, then EOF.
type slowReader struct {
	chunks [][]byte
	gap    time.Duration
	i      int
	closed atomic.Bool
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if s.i >= len(s.chunks) {
		return 0, io.EOF
	}
	time.Sleep(s.gap)
	if s.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	n := copy(p, s.chunks[s.i])
	s.i++
	return n, nil
}
func (s *slowReader) Close() error { s.closed.Store(true); return nil }

func TestStreamCopy_LiveStreamCompletesPastFixedWindow(t *testing.T) {
	// Chunks arrive every 20ms; total ~100ms exceeds a naive short deadline, but
	// each gap is under the idle window, so the whole stream must be delivered.
	body := &slowReader{
		chunks: [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d"), []byte("e")},
		gap:    20 * time.Millisecond,
	}
	rec := httptest.NewRecorder()
	streamCopy(context.Background(), rec, body, 200*time.Millisecond, 0)
	if got := rec.Body.String(); got != "abcde" {
		t.Fatalf("expected full stream 'abcde', got %q", got)
	}
}

// blockingReader never returns data until closed.
type blockingReader struct {
	ch     chan struct{}
	closed atomic.Bool
}

func newBlockingReader() *blockingReader { return &blockingReader{ch: make(chan struct{})} }
func (b *blockingReader) Read(p []byte) (int, error) {
	<-b.ch
	return 0, io.ErrClosedPipe
}
func (b *blockingReader) Close() error {
	if b.closed.CompareAndSwap(false, true) {
		close(b.ch)
	}
	return nil
}

func TestStreamCopy_IdleTimeoutAbortsStalledStream(t *testing.T) {
	body := newBlockingReader()
	rec := httptest.NewRecorder()
	start := time.Now()
	done := make(chan struct{})
	go func() {
		streamCopy(context.Background(), rec, body, 60*time.Millisecond, 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("streamCopy did not abort a stalled stream on idle timeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("idle abort took too long: %v", elapsed)
	}
	if !body.closed.Load() {
		t.Fatal("streamCopy must Close the body on idle abort (to unblock the reader)")
	}
}

func TestStreamCopy_ContextCancelAborts(t *testing.T) {
	body := newBlockingReader()
	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { streamCopy(ctx, rec, body, time.Hour, 0); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("streamCopy did not honour context cancellation")
	}
	if !body.closed.Load() {
		t.Fatal("body should be closed on context cancel")
	}
}

// --- F5: single-flight half-open probe ---

func TestTryStartAttempt_SingleFlightHalfOpenProbe(t *testing.T) {
	s := &RouterService{
		config:        breakerConfig{RequestTimeout: 45 * time.Second},
		modelBreakers: map[string]*modelBreakerState{},
	}
	attempt := modelAttempt{Profile: "coding", Model: "m1"}
	key := s.breakerKey(attempt)
	// Circuit opened but cooldown already elapsed → half-open window.
	s.modelBreakers[key] = &modelBreakerState{OpenUntil: time.Now().Add(-time.Second)}

	const N = 32
	var trues atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if s.tryStartAttempt(attempt) {
				trues.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := trues.Load(); got != 1 {
		t.Fatalf("expected exactly one probe to win the half-open window, got %d", got)
	}

	// A success clears the probe, re-allowing the model (now closed).
	s.recordSuccess(attempt)
	if !s.tryStartAttempt(attempt) {
		t.Fatal("after recordSuccess the closed circuit should allow attempts")
	}
}

func TestTryStartAttempt_ClosedCircuitFullyConcurrent(t *testing.T) {
	s := &RouterService{
		config:        breakerConfig{RequestTimeout: 45 * time.Second},
		modelBreakers: map[string]*modelBreakerState{},
	}
	attempt := modelAttempt{Profile: "coding", Model: "closed"}
	// A closed circuit must allow ALL concurrent callers (no probe throttling).
	const N = 32
	var allowed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if s.tryStartAttempt(attempt) {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := allowed.Load(); got != N {
		t.Fatalf("closed circuit should allow all %d concurrent callers, got %d", N, got)
	}
}

// TestProbeClearedByPenalize verifies a half-open probe that fails with a
// non-breaker-eligible error (only penalizeScore, no recordFailure) still
// releases the probe token so the next caller can probe immediately.
func TestProbeClearedByPenalize(t *testing.T) {
	s := &RouterService{
		config:        breakerConfig{RequestTimeout: 45 * time.Second},
		modelBreakers: map[string]*modelBreakerState{},
	}
	attempt := modelAttempt{Profile: "coding", Model: "m1"}
	key := s.breakerKey(attempt)
	s.modelBreakers[key] = &modelBreakerState{OpenUntil: time.Now().Add(-time.Second)}

	if !s.tryStartAttempt(attempt) {
		t.Fatal("first caller should claim the probe")
	}
	if s.tryStartAttempt(attempt) {
		t.Fatal("second caller should be blocked while probe in flight")
	}
	// Probe fails with a non-breaker-eligible error (e.g. 404): only penalize.
	s.penalizeScore(attempt, 404)
	if !s.tryStartAttempt(attempt) {
		t.Fatal("after penalizeScore the probe should be released for the next caller")
	}
}

// TestHealthNoDeadlockUnderConcurrentAttempts is a regression guard for the
// recursive-RLock deadlock: Health() runs concurrently with tryStartAttempt
// (which takes the write lock) and must never wedge breakerMu.
func TestHealthNoDeadlockUnderConcurrentAttempts(t *testing.T) {
	s := NewRouterService()
	// Seed a half-open circuit so tryStartAttempt exercises the write-lock path.
	attempt := modelAttempt{Profile: s.getPolicy().DefaultProfile, Model: "seed"}
	s.modelBreakers[s.breakerKey(attempt)] = &modelBreakerState{OpenUntil: time.Now().Add(-time.Second)}

	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					_ = s.Health()
					_ = s.tryStartAttempt(attempt)
					s.recordSuccess(attempt)
				}
			}
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	time.Sleep(300 * time.Millisecond)
	close(done)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: Health()/tryStartAttempt wedged breakerMu")
	}
}

func TestTotalTimeoutExceedsPerAttemptByDefault(t *testing.T) {
	perAttempt := 45 * time.Second
	got := totalTimeout(perAttempt)
	if got <= perAttempt {
		t.Fatalf("total timeout (%v) must exceed per-attempt (%v) so F6 isn't a no-op", got, perAttempt)
	}
	if got != 120*time.Second {
		t.Fatalf("expected default 120s, got %v", got)
	}
}
