package router

import (
	"testing"
	"time"
)

// env builds a decay clock at `now` with `seq` accumulated attempts and the
// default window, so tests read as "this much time and this much evidence later".
func env(now time.Time, seq int64) scoreEnv { return newScoreEnv(now, seq) }

func TestScoreSuccessRaisesFailureLowers(t *testing.T) {
	now := time.Now()
	st := &modelBreakerState{}
	applyScoreSuccess(st, env(now, 0))
	if st.Score < 0.99 {
		t.Fatalf("success from initial should stay near 1.0, got %v", st.Score)
	}
	applyScoreFailure(st, 429, env(now, 1))
	if st.Score >= 0.99 {
		t.Fatalf("failure should lower score, got %v", st.Score)
	}
}

func TestRateLimitPenalizesHarderThanSoft(t *testing.T) {
	now := time.Now()
	hard := &modelBreakerState{}
	applyScoreFailure(hard, 429, env(now, 0)) // 1.0*(1-0.4)=0.6
	soft := &modelBreakerState{}
	applyScoreFailure(soft, 400, env(now, 0)) // 1.0*(1-0.2)=0.8
	if !(hard.Score < soft.Score) {
		t.Fatalf("429 should penalize harder: hard=%v soft=%v", hard.Score, soft.Score)
	}
}

// Decay still works — but only when BOTH clocks have run: a full window of
// wall time AND a full window of further attempts.
func TestScoreDecayRecoversTowardNeutral(t *testing.T) {
	now := time.Now()
	w := env(now, 0)
	demoted := &modelBreakerState{Score: 0.2, ScoreUpdatedAt: now.Add(-w.duration), Successes: 3}
	full := decayedScore(demoted, env(now, w.attempts))
	if full < 0.79 {
		t.Fatalf("after a full window of time AND evidence should be ~neutral(0.8), got %v", full)
	}
	half := decayedScore(
		&modelBreakerState{Score: 0.2, ScoreUpdatedAt: now.Add(-w.duration / 2), Successes: 3},
		env(now, w.attempts/2),
	)
	if half <= 0.2 || half >= 0.8 {
		t.Fatalf("after half a window should be partway, got %v", half)
	}
}

// THE regression this subsystem exists to prevent. Before the fix, decay was
// pure wall-clock over a five-minute window, so after any idle gap every score
// read exactly scoreNeutral and a model with 96 successes was indistinguishable
// from one with 0 successes and a standing failure. Nothing about a model
// changes while nobody calls it, so an idle gap must not erase what was learned.
func TestScoreSurvivesIdleGapWithNoNewEvidence(t *testing.T) {
	now := time.Now()
	// Half an hour idle — six times the old five-minute window — and not one
	// further attempt anywhere in the proxy.
	demoted := &modelBreakerState{Score: 0.2, ScoreUpdatedAt: now.Add(-30 * time.Minute), ScoreAttemptSeq: 7, Successes: 2}
	got := decayedScore(demoted, env(now, 7))
	if got != 0.2 {
		t.Fatalf("an idle gap with no new attempts must not move the score: got %v, want 0.2", got)
	}
	// And a whole day idle is still no evidence.
	demoted.ScoreUpdatedAt = now.Add(-24 * time.Hour)
	if got := decayedScore(demoted, env(now, 7)); got != 0.2 {
		t.Fatalf("24h idle with no new attempts must not move the score: got %v", got)
	}
}

// The evidence clock is the binding one: a long-elapsed window forgives only as
// fast as further attempts actually accumulate.
func TestDecayIsGatedByAccumulatedEvidence(t *testing.T) {
	now := time.Now()
	w := env(now, 0)
	state := &modelBreakerState{Score: 0.2, ScoreUpdatedAt: now.Add(-10 * w.duration), Successes: 2}
	// Ten full windows of wall time, but only a tenth of a window of evidence.
	got := decayedScore(state, env(now, w.attempts/10))
	want := 0.2 + (scoreNeutral-0.2)*0.1
	if got < want-0.01 || got > want+0.01 {
		t.Fatalf("decay should track evidence, not elapsed time: got %v, want ~%v", got, want)
	}
}

// End-to-end on the ranking path: after a realistic idle gap, the chain must
// still prefer the model it learned was good.
func TestRankStillReordersAfterIdleGap(t *testing.T) {
	t.Setenv("PROXY_SCORING_ENABLED", "true")
	s := &RouterService{modelBreakers: map[string]*modelBreakerState{}}
	stale := time.Now().Add(-30 * time.Minute)
	s.modelBreakers["coding:slow"] = &modelBreakerState{Score: 0.2, ScoreUpdatedAt: stale, Successes: 1}
	s.modelBreakers["coding:good"] = &modelBreakerState{Score: 0.95, ScoreUpdatedAt: stale, Successes: 96}
	attempts := []modelAttempt{
		{Profile: "coding", Model: "slow"}, // first in policy order
		{Profile: "coding", Model: "good"},
	}
	ranked := s.rankAttemptsByScore(attempts)
	if ranked[0].Model != "good" {
		t.Fatalf("after a 30m idle gap the proven model must still rank first, got %q", ranked[0].Model)
	}
}

// A model that has never once succeeded is not the same as one that had a bad
// day: it has produced no counter-evidence at all, so it drifts back to a lower
// baseline and stays behind a model that actually recovered.
func TestNeverSucceededDriftsToLowerBaseline(t *testing.T) {
	now := time.Now()
	w := env(now, 0)
	unproven := &modelBreakerState{Score: 0.1, ScoreUpdatedAt: now.Add(-w.duration), Successes: 0}
	proven := &modelBreakerState{Score: 0.1, ScoreUpdatedAt: now.Add(-w.duration), Successes: 5}
	late := env(now, w.attempts)

	gotUnproven := decayedScore(unproven, late)
	gotProven := decayedScore(proven, late)
	if gotUnproven >= gotProven {
		t.Fatalf("a never-succeeded model must not forgive to the same baseline: unproven=%v proven=%v", gotUnproven, gotProven)
	}
	if gotUnproven < scoreUnproven-0.01 || gotUnproven > scoreUnproven+0.01 {
		t.Fatalf("unproven baseline should be %v, got %v", scoreUnproven, gotUnproven)
	}
}

// One real success moves a model off the unproven baseline for good.
func TestFirstSuccessLeavesUnprovenBaseline(t *testing.T) {
	now := time.Now()
	st := &modelBreakerState{Score: 0.1, ScoreUpdatedAt: now.Add(-time.Hour), Successes: 0}
	if base := scoreBaseline(st); base != scoreUnproven {
		t.Fatalf("zero successes should use the unproven baseline, got %v", base)
	}
	st.Successes = 1
	if base := scoreBaseline(st); base != scoreNeutral {
		t.Fatalf("after a success the baseline should be neutral, got %v", base)
	}
}

func TestRankReordersByScoreStable(t *testing.T) {
	t.Setenv("PROXY_SCORING_ENABLED", "true")
	s := &RouterService{modelBreakers: map[string]*modelBreakerState{}}
	now := time.Now()
	s.modelBreakers["coding:A"] = &modelBreakerState{Score: 0.2, ScoreUpdatedAt: now, Successes: 1}
	s.modelBreakers["coding:C"] = &modelBreakerState{Score: 0.6, ScoreUpdatedAt: now, Successes: 1}
	// B has no state -> initial 1.0
	attempts := []modelAttempt{
		{Profile: "coding", Model: "A"},
		{Profile: "coding", Model: "B"},
		{Profile: "coding", Model: "C"},
	}
	ranked := s.rankAttemptsByScore(attempts)
	got := ranked[0].Model + ranked[1].Model + ranked[2].Model
	if got != "BCA" {
		t.Fatalf("expected order B,C,A by score, got %s", got)
	}
}

func TestRankDisabledPreservesOrder(t *testing.T) {
	t.Setenv("PROXY_SCORING_ENABLED", "false")
	s := &RouterService{modelBreakers: map[string]*modelBreakerState{}}
	s.modelBreakers["coding:A"] = &modelBreakerState{Score: 0.1, ScoreUpdatedAt: time.Now()}
	attempts := []modelAttempt{{Profile: "coding", Model: "A"}, {Profile: "coding", Model: "B"}}
	ranked := s.rankAttemptsByScore(attempts)
	if ranked[0].Model != "A" {
		t.Fatalf("disabled scoring must preserve policy order, got %s first", ranked[0].Model)
	}
}

func TestScoringRecoveryWindowIsConfigurable(t *testing.T) {
	t.Setenv("PROXY_SCORING_RECOVERY_SECONDS", "60")
	t.Setenv("PROXY_SCORING_RECOVERY_ATTEMPTS", "4")
	w := newScoreEnv(time.Now(), 0)
	if w.duration != time.Minute {
		t.Errorf("PROXY_SCORING_RECOVERY_SECONDS ignored: got %v", w.duration)
	}
	if w.attempts != 4 {
		t.Errorf("PROXY_SCORING_RECOVERY_ATTEMPTS ignored: got %v", w.attempts)
	}
}

func TestScoringRecoveryWindowDefaultsAreWide(t *testing.T) {
	w := newScoreEnv(time.Now(), 0)
	if w.duration < time.Hour {
		t.Errorf("the default decay window must outlive a realistic idle gap, got %v", w.duration)
	}
}

// Every scored outcome must advance the proxy-wide evidence clock, or decay
// would never progress no matter how much traffic flowed.
func TestScoredOutcomesAdvanceTheEvidenceClock(t *testing.T) {
	s := &RouterService{modelBreakers: map[string]*modelBreakerState{}}
	attempt := modelAttempt{Profile: "coding", Model: "A"}
	s.recordSuccess(attempt)
	s.penalizeScore(attempt, 500)
	if got := s.scoreAttempts.Load(); got != 2 {
		t.Fatalf("expected 2 scored attempts on the evidence clock, got %d", got)
	}
	s.breakerMu.RLock()
	st := s.modelBreakers["coding:A"]
	s.breakerMu.RUnlock()
	if st.ScoreAttemptSeq != 2 {
		t.Fatalf("the state should carry the clock reading of its last update, got %d", st.ScoreAttemptSeq)
	}
}
