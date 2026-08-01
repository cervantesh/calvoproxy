package router

import (
	"sort"
	"time"
)

// Per-model reliability scoring.
//
// Each model carries a score in [0,1] that rises on success and falls on
// failure (harder on rate-limit / server errors / timeouts), and recovers
// toward a neutral baseline over time so a model that had a bad spell climbs
// back and gets retried. The score REORDERS the (breaker-filtered) model chain
// so the most reliable eligible model is tried first. The circuit breaker
// remains the hard gate that excludes an unhealthy model entirely; scoring only
// reprioritises the ones that are still eligible.
const (
	scoreInitial     = 1.0             // new/unseen models start optimistic
	scoreNeutral     = 0.8             // baseline a score drifts toward when idle
	scoreRecovery    = 5 * time.Minute // time to fully drift back to neutral
	scoreSuccessStep = 0.2             // fraction of the gap to 1.0 gained per success
	scorePenaltyHard = 0.4             // rate-limit (429) / 5xx / timeout (408)
	scorePenaltySoft = 0.2             // any other failure
	// scoreStatusSoftOnly asks penalizeScore for the SOFT step without
	// claiming an HTTP status. applyScoreFailure escalates on 408/429/5xx,
	// so handing it a real status for a non-HTTP judgement — "this model was
	// merely slow to start" — silently buys the hard penalty meant for
	// timeouts and rate limits.
	scoreStatusSoftOnly = 0
)

// scoringEnabled reports whether score-based reordering is active
// (PROXY_SCORING_ENABLED, default true).
func scoringEnabled() bool { return envBool("PROXY_SCORING_ENABLED", true) }

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

// decayedScore drifts a stored score toward scoreNeutral over scoreRecovery.
// Pure: it does not mutate state. Unseen scores (zero ScoreUpdatedAt) read as
// scoreInitial.
func decayedScore(score float64, updatedAt, now time.Time) float64 {
	if updatedAt.IsZero() {
		return scoreInitial
	}
	elapsed := now.Sub(updatedAt)
	if elapsed <= 0 {
		return score
	}
	frac := float64(elapsed) / float64(scoreRecovery)
	if frac > 1 {
		frac = 1
	}
	return score + (scoreNeutral-score)*frac
}

// currentBase returns the decay-adjusted base score to fold an outcome into.
func currentBase(state *modelBreakerState, now time.Time) float64 {
	if state.ScoreUpdatedAt.IsZero() {
		return scoreInitial
	}
	return decayedScore(state.Score, state.ScoreUpdatedAt, now)
}

// applyScoreSuccess folds elapsed-time recovery and a success into the score.
// Caller holds the write lock.
func applyScoreSuccess(state *modelBreakerState, now time.Time) {
	base := currentBase(state, now)
	state.Score = clamp01(base + (1-base)*scoreSuccessStep)
	state.ScoreUpdatedAt = now
}

// applyScoreFailure folds elapsed-time recovery and a failure into the score,
// penalising rate-limit / server / timeout errors harder. Caller holds the
// write lock.
func applyScoreFailure(state *modelBreakerState, statusCode int, now time.Time) {
	base := currentBase(state, now)
	penalty := scorePenaltySoft
	if statusCode == 429 || statusCode == 408 || statusCode >= 500 {
		penalty = scorePenaltyHard
	}
	state.Score = clamp01(base * (1 - penalty))
	state.ScoreUpdatedAt = now
}

// penalizeScore lowers a model's reliability score after a failed attempt.
// Unlike the circuit breaker (which only reacts to breaker-eligible errors),
// scoring reacts to EVERY failure — including "model unavailable" 404s from a
// retired free slug — so a consistently failing model sinks to the back of the
// chain even when it never trips the breaker.
func (s *RouterService) penalizeScore(attempt modelAttempt, statusCode int) {
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	key := s.breakerKey(attempt)
	state := s.modelBreakers[key]
	if state == nil {
		state = &modelBreakerState{}
		s.modelBreakers[key] = state
	}
	// Clear any half-open probe claim: penalizeScore runs on EVERY failure,
	// including non-breaker-eligible ones (4xx, truncated/oversize) that never
	// reach recordFailure. Without this, a probe that failed on such an error
	// would keep the model soft-skipped until its TTL lapses (~45s) instead of
	// immediately re-arming the next probe.
	state.ProbeUntil = time.Time{}
	applyScoreFailure(state, statusCode, time.Now())
}

// scoreForAttempt returns the current (time-decayed) reliability score for a
// model; unseen models get scoreInitial. Takes the read lock.
func (s *RouterService) scoreForAttempt(attempt modelAttempt) float64 {
	// Hold the read lock for the whole read: the state pointer AND the score
	// fields it points at must be read under the lock, or a concurrent
	// recordSuccess/penalizeScore (which write those fields under the write lock)
	// races us. Releasing before dereferencing was a latent data race.
	s.breakerMu.RLock()
	defer s.breakerMu.RUnlock()
	state := s.modelBreakers[s.breakerKey(attempt)]
	if state == nil || state.ScoreUpdatedAt.IsZero() {
		return scoreInitial
	}
	return decayedScore(state.Score, state.ScoreUpdatedAt, time.Now())
}

// rankAttemptsByScore stable-sorts already breaker-filtered attempts by
// reliability score, highest first. Stable, so equal scores keep the
// policy-defined order (a fresh chain is tried exactly as configured). Returns
// the input unchanged when scoring is disabled or there's nothing to reorder.
func (s *RouterService) rankAttemptsByScore(attempts []modelAttempt) []modelAttempt {
	if !scoringEnabled() || len(attempts) < 2 {
		return attempts
	}
	score := make([]float64, len(attempts))
	for i, a := range attempts {
		score[i] = s.scoreForAttempt(a)
	}
	idx := make([]int, len(attempts))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return score[idx[a]] > score[idx[b]] })
	ranked := make([]modelAttempt, len(attempts))
	for i, j := range idx {
		ranked[i] = attempts[j]
	}
	return ranked
}
