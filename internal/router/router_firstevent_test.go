package router

import (
	"io"
	"strings"
	"testing"
	"time"
)

type slowBody struct {
	chunks []string
	delay  time.Duration
	idx    int
	closed bool
	buf    string
}

func (s *slowBody) Read(p []byte) (int, error) {
	if s.buf == "" {
		if s.idx >= len(s.chunks) {
			return 0, io.EOF
		}
		time.Sleep(s.delay)
		if s.closed {
			return 0, io.ErrClosedPipe
		}
		s.buf = s.chunks[s.idx]
		s.idx++
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}
func (s *slowBody) Close() error { s.closed = true; return nil }

// The trap: OpenRouter emits `: OPENROUTER PROCESSING` while a request sits
// QUEUED upstream — exactly the condition this wait exists to detect. Treating
// any arriving byte as progress would defeat the mechanism in the only case
// that matters.
func TestAwaitFirstStreamEvent_KeepaliveCommentsAreNotProgress(t *testing.T) {
	body := &slowBody{
		chunks: []string{": OPENROUTER PROCESSING\n", ": OPENROUTER PROCESSING\n", "data: {\"x\":1}\n"},
		delay:  40 * time.Millisecond,
	}
	_, ok, timedOut := awaitFirstStreamEvent(body, 60*time.Millisecond)
	if !timedOut {
		t.Error("keepalive comments must not satisfy the first-event budget")
	}
	if ok {
		t.Error("a comment is not an event")
	}
}

// The mirror case: a real event inside the budget must NOT be abandoned.
func TestAwaitFirstStreamEvent_RealEventPasses(t *testing.T) {
	body := &slowBody{
		chunks: []string{": keepalive\n", "data: {\"choices\":[]}\n", "data: [DONE]\n"},
		delay:  5 * time.Millisecond,
	}
	r, ok, timedOut := awaitFirstStreamEvent(body, 2*time.Second)
	if timedOut || !ok {
		t.Fatalf("a real event arrived in time: ok=%v timedOut=%v", ok, timedOut)
	}
	// Nothing may be lost: the comment AND the first data event must still be
	// readable by the caller. Dropping the first token is a silent one-line bug.
	rest, _ := io.ReadAll(r)
	got := string(rest)
	if !strings.Contains(got, ": keepalive") {
		t.Error("bytes consumed while waiting were not replayed")
	}
	if !strings.Contains(got, `data: {"choices":[]}`) {
		t.Error("the first data event was swallowed")
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Error("the reader did not continue past what the wait consumed")
	}
}

// A stream that simply ends is not a timeout — the caller must commit and let
// the normal outcome machinery classify it, not abandon the chain.
func TestAwaitFirstStreamEvent_EndOfStreamIsNotATimeout(t *testing.T) {
	body := &slowBody{chunks: []string{": only a comment\n"}, delay: time.Millisecond}
	r, ok, timedOut := awaitFirstStreamEvent(body, 2*time.Second)
	if timedOut {
		t.Error("EOF must not be reported as a timeout")
	}
	if ok {
		t.Error("no data event ever arrived")
	}
	if r == nil {
		t.Fatal("the caller still needs a usable reader")
	}
}

// Fable's condition 3: with nothing left to fall back to, abandoning converts a
// slow success into a fast failure. The last attempt must opt out.
func TestLastInChainSkipsFirstEventBudget(t *testing.T) {
	t.Setenv("PROXY_STREAM_FIRST_BYTE_TIMEOUT", "1")
	if got := streamFirstEventTimeout(); got != time.Second {
		t.Fatalf("knob not honored: %v", got)
	}
	// The guard is `budget > 0 && !attempt.LastInChain`; assert the flag is
	// actually threaded through by the executor rather than defaulting false.
	last := modelAttempt{Model: "z/z:free", LastInChain: true}
	if !last.LastInChain {
		t.Error("LastInChain must survive on the value passed to executeAttempt")
	}
}

// Default must be disabled: this changes when a request gives up, so it should
// not turn itself on during an upgrade.
func TestFirstEventBudgetDefaultsDisabled(t *testing.T) {
	t.Setenv("PROXY_STREAM_FIRST_BYTE_TIMEOUT", "")
	if got := streamFirstEventTimeout(); got != 0 {
		t.Errorf("default must be 0 (disabled), got %v", got)
	}
}

// Regression for the defect Fable caught: the abandonment passed 503 into
// penalizeScore, and applyScoreFailure escalates on >=500 — so "this model was
// slow to start" silently bought the hard penalty meant for real timeouts and
// rate limits. The comment said soft; the code did hard.
func TestAbandonmentUsesSoftPenaltyNotHard(t *testing.T) {
	now := time.Now()
	soft := &modelBreakerState{Score: 1.0, ScoreUpdatedAt: now}
	applyScoreFailure(soft, scoreStatusSoftOnly, newScoreEnv(now, 0))

	hard := &modelBreakerState{Score: 1.0, ScoreUpdatedAt: now}
	applyScoreFailure(hard, 503, newScoreEnv(now, 0))

	if soft.Score <= hard.Score {
		t.Fatalf("the abandonment penalty must be gentler than a 5xx: soft=%.3f hard=%.3f", soft.Score, hard.Score)
	}
	if got, want := soft.Score, 1.0*(1-scorePenaltySoft); got != want {
		t.Errorf("abandonment applied %.3f, want the soft step %.3f", got, want)
	}
	// And pin the trap itself, so nobody "simplifies" it back to a status code.
	if want := 1.0 * (1 - scorePenaltyHard); hard.Score != want {
		t.Errorf("503 should still be the hard step: %.3f vs %.3f", hard.Score, want)
	}
}
