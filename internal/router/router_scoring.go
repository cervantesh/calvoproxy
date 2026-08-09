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

type providerAttemptGroup struct {
	provider    providerID
	attempts    []modelAttempt
	reliability float64
	policyOrder int
}

func (s *RouterService) normalizedProvider(attempt modelAttempt) providerID {
	if attempt.Provider != "" {
		return attempt.Provider
	}
	return s.defaultPolicyProvider()
}

// rankAttemptsLocked combines two layers while breakerMu is held:
// independent model reliability inside each provider, then global provider
// balance. The global effective-use value is attempts - reliability: usage
// dominates after a one-attempt difference, while reliability decides which
// equally consumed provider goes first. This keeps healthy providers within
// one real call of each other without randomising toward the least reliable.
func (s *RouterService) rankAttemptsLocked(attempts []modelAttempt, env scoreEnv) []modelAttempt {
	groups := make([]providerAttemptGroup, 0, 3)
	groupIndex := map[providerID]int{}
	for _, attempt := range attempts {
		provider := s.normalizedProvider(attempt)
		attempt.Provider = provider
		idx, ok := groupIndex[provider]
		if !ok {
			idx = len(groups)
			groupIndex[provider] = idx
			groups = append(groups, providerAttemptGroup{provider: provider, policyOrder: idx})
		}
		groups[idx].attempts = append(groups[idx].attempts, attempt)
	}
	for i := range groups {
		if scoringEnabled() {
			sort.SliceStable(groups[i].attempts, func(a, b int) bool {
				left := decayedScore(s.modelBreakers[s.breakerKey(groups[i].attempts[a])], env)
				right := decayedScore(s.modelBreakers[s.breakerKey(groups[i].attempts[b])], env)
				return left > right
			})
		}
		groups[i].reliability = scoreInitial
		if len(groups[i].attempts) > 0 && scoringEnabled() {
			groups[i].reliability = decayedScore(s.modelBreakers[s.breakerKey(groups[i].attempts[0])], env)
		}
	}
	sort.SliceStable(groups, func(i, j int) bool {
		left := float64(s.providerBalance[groups[i].provider]) - groups[i].reliability
		right := float64(s.providerBalance[groups[j].provider]) - groups[j].reliability
		if left == right {
			return groups[i].policyOrder < groups[j].policyOrder
		}
		return left < right
	})
	ranked := make([]modelAttempt, 0, len(attempts))
	for _, group := range groups {
		ranked = append(ranked, group.attempts...)
	}
	return ranked
}

// rankAttemptsByScore is the read-only view used by tests and diagnostics.
func (s *RouterService) rankAttemptsByScore(attempts []modelAttempt) []modelAttempt {
	if len(attempts) < 2 {
		return attempts
	}
	env := s.scoreEnv()
	s.breakerMu.RLock()
	defer s.breakerMu.RUnlock()
	return s.rankAttemptsLocked(attempts, env)
}

// rankAndReserveAttempts atomically chooses and counts the request's primary
// provider. Concurrent requests therefore spread across providers instead of
// all observing the same least-used value and stampeding one upstream.
func (s *RouterService) rankAndReserveAttempts(attempts []modelAttempt) []modelAttempt {
	if len(attempts) == 0 {
		return attempts
	}
	env := s.scoreEnv()
	s.breakerMu.Lock()
	if s.providerBalance == nil {
		s.providerBalance = make(map[providerID]int64)
	}
	s.rebaseProviderBalanceLocked(attempts)
	ranked := s.rankAttemptsLocked(attempts, env)
	provider := s.normalizedProvider(ranked[0])
	s.providerBalance[provider]++
	ranked[0].BalanceReserved = true
	s.breakerMu.Unlock()
	s.markScoresDirty()
	return ranked
}

// rebaseProviderBalanceLocked prevents catch-up bursts. A provider that was
// unconfigured, cooling, or absent for a long time rejoins at most one virtual
// attempt behind the active leaders; it shares traffic from NOW rather than
// monopolising requests to repay historical usage it could not receive.
func (s *RouterService) rebaseProviderBalanceLocked(attempts []modelAttempt) {
	providers := map[providerID]struct{}{}
	maxBalance := int64(0)
	for _, attempt := range attempts {
		provider := s.normalizedProvider(attempt)
		providers[provider] = struct{}{}
		if s.providerBalance[provider] > maxBalance {
			maxBalance = s.providerBalance[provider]
		}
	}
	floor := maxBalance - 1
	if floor < 0 {
		floor = 0
	}
	for provider := range providers {
		if s.providerBalance[provider] < floor {
			s.providerBalance[provider] = floor
		}
	}
}

func (s *RouterService) reserveProviderBalance(provider providerID) {
	s.breakerMu.Lock()
	if s.providerBalance == nil {
		s.providerBalance = make(map[providerID]int64)
	}
	s.providerBalance[provider]++
	s.breakerMu.Unlock()
	s.markScoresDirty()
}

func (s *RouterService) recordProviderAttempt(provider providerID, balanceReserved bool) {
	s.breakerMu.Lock()
	if s.providerAttempts == nil {
		s.providerAttempts = make(map[providerID]int64)
	}
	if s.providerBalance == nil {
		s.providerBalance = make(map[providerID]int64)
	}
	s.providerAttempts[provider]++
	if !balanceReserved {
		s.providerBalance[provider]++
	}
	s.breakerMu.Unlock()
	s.markScoresDirty()
}

func (s *RouterService) providerAttemptCount(provider providerID) int64 {
	s.breakerMu.RLock()
	defer s.breakerMu.RUnlock()
	return s.providerAttempts[provider]
}

func (s *RouterService) providerBalanceCount(provider providerID) int64 {
	s.breakerMu.RLock()
	defer s.breakerMu.RUnlock()
	return s.providerBalance[provider]
}
