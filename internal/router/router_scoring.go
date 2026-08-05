package router

import (
	"sort"
	"time"
)

// Per-model reliability scoring.
//
// Each model carries a score in [0,1] that rises on success and falls on
// failure (harder on rate-limit / server errors / timeouts), and recovers
// toward a baseline as NEW EVIDENCE accumulates, so a model that had a bad
// spell climbs back and gets retried. The score REORDERS the (breaker-filtered)
// model chain so the most reliable eligible model is tried first. The circuit
// breaker remains the hard gate that excludes an unhealthy model entirely;
// scoring only reprioritises the ones that are still eligible.
//
// Decay is measured against TWO clocks and advances at the slower of them:
// wall time and the proxy-wide count of scored attempts. The attempt clock is
// the one that matters. Decay-by-wall-clock alone (the pre-0.9.2 behaviour,
// with a five-minute window) meant every score converged on the neutral
// baseline during any idle gap, and a bursty interactive workload is mostly
// idle gap: the chain relearned which models worked, forgot within five
// minutes, and re-paid the abandonment cost on the very next burst. Measured on
// a live instance, 53 of 183 requests (29%) were abandoned at the first-event
// budget because the chain kept re-choosing a model it had already learned was
// slow. Nothing about a model changes while nobody is calling it, so an idle
// gap must not erase what was learned; only further attempts — actual new
// evidence, for or against — should move a score back toward the baseline.
const (
	scoreInitial     = 1.0 // new/unseen models start optimistic
	scoreNeutral     = 0.8 // baseline a proven model's score drifts back toward
	scoreSuccessStep = 0.2 // fraction of the gap to 1.0 gained per success
	scorePenaltyHard = 0.4 // rate-limit (429) / 5xx / timeout (408)
	scorePenaltySoft = 0.2 // any other failure
	// scoreUnproven is the baseline for a model that has never once succeeded
	// in this proxy's memory. Such a model is NOT the same as one that had a
	// bad day: a bad day is contradicted by the successes surrounding it, while
	// "0 successes, N failures" has no counter-evidence at all. Letting it drift
	// all the way back to scoreNeutral would rank it level with a model that has
	// actually answered — which is exactly what the live instance showed
	// (gemma-4-31b: 0 successes, 1 standing failure, score 0.8000, identical to
	// a model with 96 successes). It still ranks above nothing and is still
	// tried, last; one real success moves it onto the normal baseline.
	scoreUnproven = 0.5
	// scoreStatusSoftOnly asks penalizeScore for the SOFT step without
	// claiming an HTTP status. applyScoreFailure escalates on 408/429/5xx,
	// so handing it a real status for a non-HTTP judgement — "this model was
	// merely slow to start" — silently buys the hard penalty meant for
	// timeouts and rate limits.
	scoreStatusSoftOnly = 0

	// defaultScoreRecovery is the wall-clock half of the decay window. Six hours,
	// not five minutes: it has to outlive a realistic idle gap (this workload is
	// used for a while, idle for 30+ minutes, used again) without needing an
	// operator to tune it. Override with PROXY_SCORING_RECOVERY_SECONDS.
	defaultScoreRecovery = 6 * time.Hour
	// defaultScoreRecoveryAttempts is the evidence half: how many further scored
	// attempts, proxy-wide, it takes to fully forgive a demoted model. Override
	// with PROXY_SCORING_RECOVERY_ATTEMPTS.
	defaultScoreRecoveryAttempts = 50
)

// scoringEnabled reports whether score-based reordering is active
// (PROXY_SCORING_ENABLED, default true).
func scoringEnabled() bool { return envBool("PROXY_SCORING_ENABLED", true) }

// scoreEnv is everything a decay calculation needs: the two clocks it is
// measured against (wall time and the proxy-wide scored-attempt counter) and
// the window over which a score drifts back to its baseline.
type scoreEnv struct {
	now      time.Time
	seq      int64
	duration time.Duration
	attempts int64
}

// newScoreEnv builds a scoreEnv from the current clocks, reading the window from
// the environment (PROXY_SCORING_RECOVERY_SECONDS / _ATTEMPTS).
func newScoreEnv(now time.Time, seq int64) scoreEnv {
	env := scoreEnv{
		now:      now,
		seq:      seq,
		duration: envDurationSeconds("PROXY_SCORING_RECOVERY_SECONDS", defaultScoreRecovery),
		attempts: int64(envInt("PROXY_SCORING_RECOVERY_ATTEMPTS", defaultScoreRecoveryAttempts)),
	}
	if env.duration <= 0 {
		env.duration = defaultScoreRecovery
	}
	if env.attempts < 1 {
		env.attempts = defaultScoreRecoveryAttempts
	}
	return env
}

// scoreEnv snapshots the service's decay clocks. Does NOT take breakerMu: the
// attempt counter is atomic and the window comes from the environment, so it is
// safe to call inside or outside the lock.
func (s *RouterService) scoreEnv() scoreEnv {
	return newScoreEnv(time.Now(), s.scoreAttempts.Load())
}

// countScoredAttempt records that one more attempt produced evidence and
// returns the decay clocks including it. Every scored outcome (success or
// failure, any model) advances the proxy-wide attempt clock — that is what
// makes decay track accumulated evidence rather than the passage of time.
func (s *RouterService) countScoredAttempt() scoreEnv {
	return newScoreEnv(time.Now(), s.scoreAttempts.Add(1))
}

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

// scoreBaseline is the value a state's score drifts toward: the neutral
// baseline normally, the lower unproven baseline for a model that has never
// recorded a success.
func scoreBaseline(state *modelBreakerState) float64 {
	if state.Successes == 0 {
		return scoreUnproven
	}
	return scoreNeutral
}

// decayFraction is how far a stored score has drifted toward its baseline: the
// SLOWER of the two clocks, so decay needs both elapsed time and accumulated
// evidence. An idle proxy advances neither meaningfully, which is the point.
func decayFraction(state *modelBreakerState, env scoreEnv) float64 {
	elapsed := env.now.Sub(state.ScoreUpdatedAt)
	if elapsed <= 0 {
		return 0
	}
	timeFrac := float64(elapsed) / float64(env.duration)

	evidence := env.seq - state.ScoreAttemptSeq
	if evidence <= 0 {
		return 0
	}
	attemptFrac := float64(evidence) / float64(env.attempts)

	frac := timeFrac
	if attemptFrac < frac {
		frac = attemptFrac
	}
	return clamp01(frac)
}

// decayedScore drifts a stored score toward its baseline over the decay window.
// Pure: it does not mutate state. Unseen scores (zero ScoreUpdatedAt) read as
// scoreInitial.
func decayedScore(state *modelBreakerState, env scoreEnv) float64 {
	if state == nil || state.ScoreUpdatedAt.IsZero() {
		return scoreInitial
	}
	baseline := scoreBaseline(state)
	return clamp01(state.Score + (baseline-state.Score)*decayFraction(state, env))
}

// applyScoreSuccess folds accumulated recovery and a success into the score.
// Caller holds the write lock.
func applyScoreSuccess(state *modelBreakerState, env scoreEnv) {
	base := decayedScore(state, env)
	state.Score = clamp01(base + (1-base)*scoreSuccessStep)
	state.ScoreUpdatedAt = env.now
	state.ScoreAttemptSeq = env.seq
}

// applyScoreFailure folds accumulated recovery and a failure into the score,
// penalising rate-limit / server / timeout errors harder. Caller holds the
// write lock.
func applyScoreFailure(state *modelBreakerState, statusCode int, env scoreEnv) {
	base := decayedScore(state, env)
	penalty := scorePenaltySoft
	if statusCode == 429 || statusCode == 408 || statusCode >= 500 {
		penalty = scorePenaltyHard
	}
	state.Score = clamp01(base * (1 - penalty))
	state.ScoreUpdatedAt = env.now
	state.ScoreAttemptSeq = env.seq
}

// penalizeScore lowers a model's reliability score after a failed attempt.
// Unlike the circuit breaker (which only reacts to breaker-eligible errors),
// scoring reacts to EVERY failure — including "model unavailable" 404s from a
// retired free slug — so a consistently failing model sinks to the back of the
// chain even when it never trips the breaker.
func (s *RouterService) penalizeScore(attempt modelAttempt, statusCode int) {
	env := s.countScoredAttempt()
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
	applyScoreFailure(state, statusCode, env)
	s.markScoresDirty()
}

// scoreForAttempt returns the current (decayed) reliability score for a model;
// unseen models get scoreInitial. Takes the read lock.
func (s *RouterService) scoreForAttempt(attempt modelAttempt, env scoreEnv) float64 {
	// Hold the read lock for the whole read: the state pointer AND the score
	// fields it points at must be read under the lock, or a concurrent
	// recordSuccess/penalizeScore (which write those fields under the write lock)
	// races us. Releasing before dereferencing was a latent data race.
	s.breakerMu.RLock()
	defer s.breakerMu.RUnlock()
	return decayedScore(s.modelBreakers[s.breakerKey(attempt)], env)
}

// rankAttemptsByScore stable-sorts already breaker-filtered attempts by
// reliability score, highest first. Stable, so equal scores keep the
// policy-defined order (a fresh chain is tried exactly as configured). Returns
// the input unchanged when scoring is disabled or there's nothing to reorder.
func (s *RouterService) rankAttemptsByScore(attempts []modelAttempt) []modelAttempt {
	return s.rankAttemptsByScoreTraced(nil, attempts)
}

// rankAttemptsByScoreTraced is rankAttemptsByScore plus the trace annotation.
// The scores are captured here rather than recomputed by the caller: this is the
// one place they already exist, and a second pass would double the locking on
// the hot path.
func (s *RouterService) rankAttemptsByScoreTraced(trace *routeTrace, attempts []modelAttempt) []modelAttempt {
	if !scoringEnabled() || len(attempts) < 2 {
		return attempts
	}
	env := s.scoreEnv()
	score := make([]float64, len(attempts))
	for i, a := range attempts {
		score[i] = s.scoreForAttempt(a, env)
	}
	if trace != nil {
		models := make([]string, len(attempts))
		for i, a := range attempts {
			models[i] = a.Model
		}
		trace.recordScores(models, score)
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
