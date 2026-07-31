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
	// Half-open: once the cooldown has elapsed the next attempt is a probe.
	// Reset the counter first so a single probe failure doesn't immediately
	// re-open the circuit — it takes a full threshold of failures again.
	if !state.OpenUntil.IsZero() && time.Now().After(state.OpenUntil) {
		state.ConsecutiveFailures = 0
		state.OpenUntil = time.Time{}
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
		slog.Warn("[CalvoProxy] 🔴 Circuit OPEN", slog.String("breaker_key", breakerKey), slog.String("open_until", state.OpenUntil.Format(time.RFC3339)))
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
	applyScoreSuccess(state, time.Now())
}

func (s *RouterService) breakerKey(attempt modelAttempt) string {
	profile := strings.TrimSpace(attempt.Profile)
	if profile == "" {
		profile = s.getPolicy().DefaultProfile
	}
	return profile + ":" + strings.TrimSpace(attempt.Model)
}

func (s *RouterService) allKnownAttempts() []modelAttempt {
	seen := map[string]struct{}{}
	attempts := make([]modelAttempt, 0)
	for profile, chain := range s.getPolicy().Profiles {
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
			Score:               decayedScore(state.Score, state.ScoreUpdatedAt, now),
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
		Service:            "CalvoProxy",
		Status:             status,
		Ready:              ready,
		OpenCircuitCount:   openCount,
		ConfiguredAPIKey:   envValue("OPENROUTER_API_KEY") != "",
		DefaultExecutor:    string(s.defaultPolicyProvider()),
		Profiles:           profileNames(s.getPolicy().Profiles),
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

// GlobalBreakerTransport is an http.RoundTripper with a PER-HOST circuit
// breaker: each upstream host (openrouter.ai, an ollama sidecar, …) has its own
// failure counter and open state, so one dead host can't blackhole traffic to
// the others.
type GlobalBreakerTransport struct {
	Base             http.RoundTripper
	FailureThreshold int
	Cooldown         time.Duration

	mu    sync.Mutex
	hosts map[string]*hostBreakerState
}

type hostBreakerState struct {
	failures  int
	openUntil time.Time
}

func (t *GlobalBreakerTransport) hostState(host string) *hostBreakerState {
	if t.hosts == nil {
		t.hosts = make(map[string]*hostBreakerState)
	}
	hb := t.hosts[host]
	if hb == nil {
		hb = &hostBreakerState{}
		t.hosts[host] = hb
	}
	return hb
}

func (t *GlobalBreakerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Host

	t.mu.Lock()
	hb := t.hostState(host)
	open := time.Now().Before(hb.openUntil)
	t.mu.Unlock()
	if open {
		return nil, fmt.Errorf("circuit breaker open for host %s: temporarily unreachable", host)
	}

	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	resp, err := base.RoundTrip(req)

	t.mu.Lock()
	defer t.mu.Unlock()
	hb = t.hostState(host)

	if err != nil {
		hb.failures++
		if hb.failures >= t.FailureThreshold {
			hb.openUntil = time.Now().Add(t.Cooldown)
			slog.Error("[CalvoProxy] 🚨 HOST CIRCUIT OPEN: host is down", slog.String("host", host), slog.String("open_until", hb.openUntil.Format(time.RFC3339)))
		}
		return resp, err
	}

	if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout {
		hb.failures++
		if hb.failures >= t.FailureThreshold {
			hb.openUntil = time.Now().Add(t.Cooldown)
			slog.Error("[CalvoProxy] 🚨 HOST CIRCUIT OPEN: host is returning errors", slog.String("host", host), slog.Int("http_code", resp.StatusCode), slog.String("open_until", hb.openUntil.Format(time.RFC3339)))
		}
	} else if resp.StatusCode < 500 {
		hb.failures = 0
		hb.openUntil = time.Time{}
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
