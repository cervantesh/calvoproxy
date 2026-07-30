package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	cervohttpkit "github.com/cervantesh/cervo-httpkit"
	cervoobserve "github.com/cervantesh/cervo-observe"
	cervoretry "github.com/cervantesh/cervo-retry"
	cervorules "github.com/cervantesh/cervo-rules/v3/core"
	"go.opentelemetry.io/otel/codes"
)

// ---
// Per-model circuit breaker state and lifecycle
// ---

func (s *RouterService) isModelAvailable(attempt modelAttempt) bool {
	s.breakerMu.RLock()
	state := s.modelBreakers[s.breakerKey(attempt)]
	s.breakerMu.RUnlock()
	if state == nil {
		return true
	}
	return !state.OpenUntil.After(time.Now())
}

func (s *RouterService) recordFailure(attempt modelAttempt, statusCode int, reason string) {
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()

	breakerKey := s.breakerKey(attempt)
	state := s.modelBreakers[breakerKey]
	if state == nil {
		state = &modelBreakerState{}
		s.modelBreakers[breakerKey] = state
	}
	state.ConsecutiveFailures++
	state.LastFailureCode = statusCode
	state.LastFailureReason = truncateReason(reason)
	state.LastFailureAt = time.Now()
	threshold := s.config.FailureThreshold
	cooldown := s.config.Cooldown
	if attempt.BreakerPolicy.FailureThreshold > 0 {
		threshold = attempt.BreakerPolicy.FailureThreshold
	}
	if attempt.BreakerPolicy.Cooldown > 0 {
		cooldown = attempt.BreakerPolicy.Cooldown
	}
	if state.ConsecutiveFailures >= threshold {
		state.OpenUntil = time.Now().Add(cooldown)
		slog.Warn("[CervoProxy] 🔴 Circuit OPEN", slog.String("breaker_key", breakerKey), slog.String("open_until", state.OpenUntil.Format(time.RFC3339)))
	}
}

func (s *RouterService) recordSuccess(attempt modelAttempt) {
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()

	breakerKey := s.breakerKey(attempt)
	state := s.modelBreakers[breakerKey]
	if state == nil {
		state = &modelBreakerState{}
		s.modelBreakers[breakerKey] = state
	}
	state.ConsecutiveFailures = 0
	state.Successes++
	state.OpenUntil = time.Time{}
	state.LastFailureCode = 0
	state.LastFailureReason = ""
	state.LastFailureAt = time.Time{}
}

func (s *RouterService) breakerKey(attempt modelAttempt) string {
	profile := strings.TrimSpace(attempt.Profile)
	if profile == "" {
		profile = s.policy.DefaultProfile
	}
	return profile + ":" + strings.TrimSpace(attempt.Model)
}

func (s *RouterService) allKnownAttempts() []modelAttempt {
	seen := map[string]struct{}{}
	attempts := make([]modelAttempt, 0)
	for profile, chain := range s.policy.Profiles {
		for _, model := range chain {
			candidate := modelAttempt{Profile: profile, Model: model, Provider: s.defaultPolicyProvider(), BreakerPolicy: s.defaultBreakerPolicy()}
			if _, ok := seen[s.breakerKey(candidate)]; ok {
				continue
			}
			seen[s.breakerKey(candidate)] = struct{}{}
			attempts = append(attempts, candidate)
		}
	}
	sort.Slice(attempts, func(i, j int) bool {
		if attempts[i].Profile == attempts[j].Profile {
			return attempts[i].Model < attempts[j].Model
		}
		return attempts[i].Profile < attempts[j].Profile
	})
	return attempts
}

func (s *RouterService) filterAvailableAttempts(attempts []modelAttempt) []modelAttempt {
	available := make([]modelAttempt, 0, len(attempts))
	for _, attempt := range attempts {
		if s.isModelAvailable(attempt) {
			available = append(available, attempt)
		}
	}
	return available
}

// Health returns a ProxyHealth snapshot of all circuit breakers.
func (s *RouterService) Health() ProxyHealth {
	s.breakerMu.RLock()
	defer s.breakerMu.RUnlock()

	snapshots := make([]BreakerSnapshot, 0, len(s.modelBreakers))
	openCount := 0
	now := time.Now()
	for model, state := range s.modelBreakers {
		snapshot := BreakerSnapshot{
			Model:               model,
			ConsecutiveFailures: state.ConsecutiveFailures,
			Successes:           state.Successes,
			LastFailureCode:     state.LastFailureCode,
			LastFailureReason:   state.LastFailureReason,
			LastFailureAt:       state.LastFailureAt,
			OpenUntil:           state.OpenUntil,
			State:               "closed",
		}
		if state.OpenUntil.After(now) {
			snapshot.State = "open"
			openCount++
		} else if !state.OpenUntil.IsZero() {
			snapshot.State = "half-open"
		}
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Model < snapshots[j].Model })

	status := "ok"
	ready := true
	if openCount > 0 {
		status = "degraded"
	}
	if len(s.filterAvailableAttempts(s.allKnownAttempts())) == 0 {
		ready = false
		status = "unavailable"
	}
	if s.modelStrict && len(s.modelWarnings) > 0 {
		ready = false
		status = "unavailable"
	}

	return ProxyHealth{
		Service:            "CervoProxy",
		Status:             status,
		Ready:              ready,
		OpenCircuitCount:   openCount,
		ConfiguredAPIKey:   envValue("OPENROUTER_API_KEY") != "",
		DefaultExecutor:    string(s.defaultPolicyProvider()),
		Profiles:           profileNames(s.policy.Profiles),
		FailureThreshold:   s.config.FailureThreshold,
		CooldownSeconds:    int(s.config.Cooldown.Seconds()),
		RequestTimeoutSecs: int(s.config.RequestTimeout.Seconds()),
		PolicyName:         s.policyMetadata.Name,
		PolicyDSLVersion:   s.policyMetadata.DSLVersion,
		PolicyHash:         s.policyMetadata.PolicyHash,
		PolicyVocabHash:    s.policyMetadata.VocabularyHash,
		ModelPolicy:        s.ModelPolicyHealth(),
		Circuits:           snapshots,
		Timestamp:          now,
	}
}

func (s *RouterService) defaultPolicyProvider() cervorules.Executor {
	if s.runtimeConfig.DefaultExecutor != "" {
		return s.runtimeConfig.DefaultExecutor
	}
	return providerOpenRouter
}

func (s *RouterService) defaultBreakerPolicy() BreakerPolicy {
	if s.runtimeConfig.BreakerPolicy.FailureThreshold > 0 {
		return s.runtimeConfig.BreakerPolicy
	}
	return BreakerPolicy{FailureThreshold: s.config.FailureThreshold, Cooldown: s.config.Cooldown, Eligible: true}
}

func profileNames(profiles map[string][]string) []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *RouterService) Ready() bool {
	return s.Health().Ready
}

// GlobalBreakerTransport is an http.RoundTripper with a global circuit breaker.
type GlobalBreakerTransport struct {
	Base             http.RoundTripper
	FailureThreshold int
	Cooldown         time.Duration

	mu        sync.RWMutex
	failures  int
	openUntil time.Time
}

func (t *GlobalBreakerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.RLock()
	open := time.Now().Before(t.openUntil)
	t.mu.RUnlock()

	if open {
		return nil, fmt.Errorf("global circuit breaker open: host is temporarily unreachable")
	}

	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	resp, err := base.RoundTrip(req)

	t.mu.Lock()
	defer t.mu.Unlock()

	if err != nil {
		t.failures++
		if t.failures >= t.FailureThreshold {
			t.openUntil = time.Now().Add(t.Cooldown)
			slog.Error("[CervoProxy] 🚨 GLOBAL CIRCUIT OPEN: Host is down", slog.String("open_until", t.openUntil.Format(time.RFC3339)))
		}
		return resp, err
	}

	if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout {
		t.failures++
		if t.failures >= t.FailureThreshold {
			t.openUntil = time.Now().Add(t.Cooldown)
			slog.Error("[CervoProxy] 🚨 GLOBAL CIRCUIT OPEN: Host is returning errors", slog.Int("http_code", resp.StatusCode), slog.String("open_until", t.openUntil.Format(time.RFC3339)))
		}
	} else if resp.StatusCode < 500 {
		t.failures = 0
		t.openUntil = time.Time{}
	}

	return resp, err
}

// --- Error classifiers ---

func classifyTransportError(err error) *attemptError {
	classification := cervoretry.ClassifyTransportError(err)
	timeout := errors.Is(err, context.DeadlineExceeded)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		timeout = true
	}
	lowerMessage := strings.ToLower(classification.Message)
	return &attemptError{
		StatusCode:      classification.StatusCode,
		Message:         classification.Message,
		BreakerEligible: classification.BreakerEligible,
		Retryable:       classification.Retryable,
		Timeout:         timeout,
		EOF:             strings.Contains(lowerMessage, "eof") || strings.Contains(lowerMessage, "connection reset"),
	}
}

func classifyHTTPError(statusCode int, responseBody string) *attemptError {
	classification := cervoretry.ClassifyHTTPStatus(statusCode, responseBody)
	return &attemptError{
		StatusCode:      classification.StatusCode,
		Message:         classification.Message,
		BreakerEligible: classification.BreakerEligible,
		Retryable:       classification.Retryable,
	}
}

// --- Telemetry helpers ---

func writeJSONError(w http.ResponseWriter, statusCode int, message string) {
	cervohttpkit.JSONError(w, statusCode, message)
}

func truncateReason(reason string) string {
	return cervoobserve.Truncate(reason, 240)
}

// Ensure codes is referenced
var _ = codes.Error
