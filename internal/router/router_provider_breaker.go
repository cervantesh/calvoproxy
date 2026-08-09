package router

import (
	"time"
)

func (s *RouterService) isProviderAvailableLocked(provider providerID) bool {
	state := s.providerBreakers[provider]
	return state == nil || !state.OpenUntil.After(time.Now())
}

func (s *RouterService) tryStartProvider(attempt modelAttempt) bool {
	provider := attempt.Provider
	s.breakerMu.RLock()
	state := s.providerBreakers[provider]
	if state == nil || state.OpenUntil.IsZero() {
		s.breakerMu.RUnlock()
		return true
	}
	if time.Now().Before(state.OpenUntil) {
		s.breakerMu.RUnlock()
		return false
	}
	s.breakerMu.RUnlock()

	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	state = s.providerBreakers[provider]
	if state == nil || state.OpenUntil.IsZero() {
		return true
	}
	now := time.Now()
	if now.Before(state.OpenUntil) || now.Before(state.ProbeUntil) {
		return false
	}
	ttl := s.config.RequestTimeout
	if ttl <= 0 {
		ttl = 45 * time.Second
	}
	state.ProbeUntil = now.Add(ttl)
	return true
}

func (s *RouterService) resolveProviderSuccess(provider providerID) {
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	if s.providerBreakers == nil {
		s.providerBreakers = make(map[providerID]*modelBreakerState)
	}
	state := s.providerBreakers[provider]
	if state == nil {
		state = &modelBreakerState{}
		s.providerBreakers[provider] = state
	}
	state.ConsecutiveFailures = 0
	state.Successes++
	state.OpenUntil = time.Time{}
	state.ProbeUntil = time.Time{}
	state.LastFailureCode = 0
	state.LastFailureReason = ""
	state.LastFailureAt = time.Time{}
}

func (s *RouterService) resolveProviderProbe(provider providerID) {
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	if state := s.providerBreakers[provider]; state != nil {
		state.ProbeUntil = time.Time{}
	}
}

func (s *RouterService) recordProviderFailure(attempt modelAttempt, statusCode int, reason string, immediateOpen bool, retryAfter time.Duration) {
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	if s.providerBreakers == nil {
		s.providerBreakers = make(map[providerID]*modelBreakerState)
	}
	state := s.providerBreakers[attempt.Provider]
	if state == nil {
		state = &modelBreakerState{}
		s.providerBreakers[attempt.Provider] = state
	}
	now := time.Now()
	if !state.OpenUntil.IsZero() && now.After(state.OpenUntil) {
		state.ConsecutiveFailures = 0
		state.OpenUntil = time.Time{}
	}
	state.ProbeUntil = time.Time{}
	state.ConsecutiveFailures++
	state.LastFailureCode = statusCode
	state.LastFailureReason = truncateReason(reason)
	state.LastFailureAt = now
	threshold := s.config.FailureThreshold
	if attempt.BreakerPolicy.FailureThreshold > 0 {
		threshold = attempt.BreakerPolicy.FailureThreshold
	}
	if immediateOpen || state.ConsecutiveFailures >= threshold {
		cooldown := s.config.Cooldown
		if attempt.BreakerPolicy.Cooldown > 0 {
			cooldown = attempt.BreakerPolicy.Cooldown
		}
		if retryAfter > cooldown {
			cooldown = retryAfter
		}
		until := now.Add(cooldown)
		if until.After(state.OpenUntil) {
			state.OpenUntil = until
		}
	}
}

func (s *RouterService) providerBreakerSnapshot(provider providerID) BreakerSnapshot {
	s.breakerMu.RLock()
	defer s.breakerMu.RUnlock()
	state := s.providerBreakers[provider]
	snapshot := BreakerSnapshot{Model: "provider:" + string(provider), State: "closed"}
	if state == nil {
		return snapshot
	}
	snapshot.ConsecutiveFailures = state.ConsecutiveFailures
	snapshot.Successes = state.Successes
	snapshot.LastFailureCode = state.LastFailureCode
	snapshot.LastFailureReason = state.LastFailureReason
	snapshot.LastFailureAt = state.LastFailureAt
	snapshot.OpenUntil = state.OpenUntil
	if state.OpenUntil.After(time.Now()) {
		snapshot.State = "open"
	} else if !state.OpenUntil.IsZero() {
		snapshot.State = "half-open"
	}
	return snapshot
}
