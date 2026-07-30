package router

import (
	"testing"
	"time"
)

func TestScoreSuccessRaisesFailureLowers(t *testing.T) {
	now := time.Now()
	st := &modelBreakerState{}
	applyScoreSuccess(st, now)
	if st.Score < 0.99 {
		t.Fatalf("success from initial should stay near 1.0, got %v", st.Score)
	}
	applyScoreFailure(st, 429, now)
	if st.Score >= 0.99 {
		t.Fatalf("failure should lower score, got %v", st.Score)
	}
}

func TestRateLimitPenalizesHarderThanSoft(t *testing.T) {
	now := time.Now()
	hard := &modelBreakerState{}
	applyScoreFailure(hard, 429, now) // 1.0*(1-0.4)=0.6
	soft := &modelBreakerState{}
	applyScoreFailure(soft, 400, now) // 1.0*(1-0.2)=0.8
	if !(hard.Score < soft.Score) {
		t.Fatalf("429 should penalize harder: hard=%v soft=%v", hard.Score, soft.Score)
	}
}

func TestScoreDecayRecoversTowardNeutral(t *testing.T) {
	now := time.Now()
	full := decayedScore(0.2, now.Add(-scoreRecovery), now)
	if full < 0.79 {
		t.Fatalf("after full window should be ~neutral(0.8), got %v", full)
	}
	half := decayedScore(0.2, now.Add(-scoreRecovery/2), now)
	if half <= 0.2 || half >= 0.8 {
		t.Fatalf("after half window should be partway, got %v", half)
	}
}

func TestRankReordersByScoreStable(t *testing.T) {
	t.Setenv("PROXY_SCORING_ENABLED", "true")
	s := &RouterService{modelBreakers: map[string]*modelBreakerState{}}
	now := time.Now()
	s.modelBreakers["coding:A"] = &modelBreakerState{Score: 0.2, ScoreUpdatedAt: now}
	s.modelBreakers["coding:C"] = &modelBreakerState{Score: 0.6, ScoreUpdatedAt: now}
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
