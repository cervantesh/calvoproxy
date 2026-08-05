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
	defer s.breakerMu.RUnlock()
	return s.isModelAvailableLocked(attempt)
}

// isModelAvailableLocked is the lock-free core: the caller MUST already hold
// breakerMu (read or write). Used both by isModelAvailable (request path, its
// own RLock) and by Health (which holds one RLock for the whole snapshot), so
// availability is never computed via a second, re-entrant RLock — Go's RWMutex
// is not recursive and a nested RLock deadlocks whenever a writer is waiting.
func (s *RouterService) isModelAvailableLocked(attempt modelAttempt) bool {
	// Quota is consulted here so hard exclusion inherits the breaker's own choke
	// points (filterAvailableAttempts, Health readiness) without sharing its
	// STATE — writing a quota window into OpenUntil would be erased by the next
	// recordSuccess and would report a spent budget as a broken circuit.
	// Lock order: the caller already holds breakerMu, and the ledger never calls
	// back into the breaker, so breakerMu -> quota.mu is the only direction.
	if s.quota.exhausted(attempt) {
		return false
	}
	state := s.modelBreakers[s.breakerKey(attempt)]
	if state == nil {
		return true
	}
	return !state.OpenUntil.After(time.Now())
}

// tryStartAttempt single-flights the half-open recovery probe. It is called on
// the hot request path immediately before an attempt actually runs (NOT from
// Health/filter, which stay read-only). Return values:
//   - closed circuit  → true  (full concurrency, no probe bookkeeping)
//   - open (cooling)   → false (should already be filtered out; belt-and-braces)
//   - half-open window → true for exactly ONE caller, which claims the probe with
//     a short TTL; concurrent callers get false until the probe resolves (any
//     record/penalize path clears it) or the TTL lapses and re-arms it.
//
// The common case (closed/nil circuit) is served under a READ lock so normal
// traffic isn't serialized; only the rare half-open claim upgrades to a write
// lock, re-checking state under it (double-checked locking).
func (s *RouterService) tryStartAttempt(attempt modelAttempt) bool {
	key := s.breakerKey(attempt)

	s.breakerMu.RLock()
	state := s.modelBreakers[key]
	if state == nil || state.OpenUntil.IsZero() {
		s.breakerMu.RUnlock()
		return true // closed → fully concurrent, no write lock needed
	}
	if time.Now().Before(state.OpenUntil) {
		s.breakerMu.RUnlock()
		return false // still cooling down
	}
	s.breakerMu.RUnlock()

	// Half-open: claim the single probe under the write lock, re-checking state.
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	state = s.modelBreakers[key]
	if state == nil || state.OpenUntil.IsZero() {
		return true
	}
	now := time.Now()
	if now.Before(state.OpenUntil) {
		return false
	}
	if now.Before(state.ProbeUntil) {
		return false // another caller already claimed the probe
	}
	ttl := s.config.RequestTimeout
	if ttl <= 0 {
		ttl = 45 * time.Second
	}
	state.ProbeUntil = now.Add(ttl)
	return true
}

// recordFailure counts a failure and opens the circuit at threshold. An optional
// retryAfter (from an upstream 429/503 Retry-After header) extends the cooldown
// so the breaker respects the server's requested backoff rather than skipping a
// still-rate-limited model back in early.
func (s *RouterService) recordFailure(attempt modelAttempt, statusCode int, reason string, retryAfter ...time.Duration) {
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
	state.ProbeUntil = time.Time{} // probe (if any) resolved
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
	// Honour an upstream Retry-After that asks for a longer wait than our cooldown.
	if len(retryAfter) > 0 && retryAfter[0] > cooldown {
		cooldown = retryAfter[0]
	}
	if state.ConsecutiveFailures >= threshold {
		// Never SHORTEN an already-open window: a later failure with the default
		// cooldown must not undo a long Retry-After that a prior 429 set.
		until := time.Now().Add(cooldown)
		if until.After(state.OpenUntil) {
			state.OpenUntil = until
		}
		slog.Warn("[CalvoProxy] 🔴 Circuit OPEN", slog.String("breaker_key", breakerKey), slog.String("open_until", state.OpenUntil.Format(time.RFC3339)))
	}
}

// resolveProbe closes the breaker bookkeeping for an attempt that has reached a
// good response (200 + headers) WITHOUT scoring it as a completed success. It is
// what a streaming attempt calls at header time: the model clearly answered, so
// the circuit and any half-open probe claim must be released immediately (leaving
// them held for the whole stream would wedge single-flight recovery and let a
// second probe stampede once the TTL lapsed). The reliability score is only
// updated later, when the stream actually ends — cleanly or not.
func (s *RouterService) resolveProbe(attempt modelAttempt) {
	s.breakerMu.Lock()
	defer s.breakerMu.Unlock()
	breakerKey := s.breakerKey(attempt)
	state := s.modelBreakers[breakerKey]
	if state == nil {
		state = &modelBreakerState{}
		s.modelBreakers[breakerKey] = state
	}
	state.ConsecutiveFailures = 0
	state.OpenUntil = time.Time{}
	state.ProbeUntil = time.Time{}
	state.LastFailureCode = 0
	state.LastFailureReason = ""
	state.LastFailureAt = time.Time{}
}

func (s *RouterService) recordSuccess(attempt modelAttempt) {
	env := s.countScoredAttempt()
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
	state.ProbeUntil = time.Time{} // probe resolved successfully
	state.LastFailureCode = 0
	state.LastFailureReason = ""
	state.LastFailureAt = time.Time{}
	applyScoreSuccess(state, env)
	s.markScoresDirty()
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

// retryAfterForAttempts returns how long until the SOONEST of these models leaves
// its cooldown, so an "all models cooling down" 503 can carry a Retry-After and
// clients back off instead of stampeding. Returns 0 when nothing is open (the
// chain was empty for another reason) — callers then omit the header.
func (s *RouterService) retryAfterForAttempts(attempts []modelAttempt) time.Duration {
	s.breakerMu.RLock()
	defer s.breakerMu.RUnlock()
	now := time.Now()
	var soonest time.Duration
	for _, attempt := range attempts {
		state := s.modelBreakers[s.breakerKey(attempt)]
		if state == nil || !state.OpenUntil.After(now) {
			continue // this model isn't the reason we're empty
		}
		d := state.OpenUntil.Sub(now)
		if soonest == 0 || d < soonest {
			soonest = d
		}
	}
	// A chain emptied by quota rather than by breakers still owes the client a
	// Retry-After; without this the 503 says "come back sometime".
	for _, attempt := range attempts {
		if d := s.quota.retryAfter(attempt); d > 0 && (soonest == 0 || d < soonest) {
			soonest = d
		}
	}
	return soonest
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

// healthFacts returns ONLY the three fields the policy engine actually consumes
// (status/ready/open-circuit count), computed under a single read lock with no
// sorting or per-circuit allocation.
//
// The policy path runs on every request; calling the full Health() there meant
// building and sorting a snapshot of every circuit — holding breakerMu (and thus
// delaying recordSuccess/recordFailure writers) purely for observability data
// that was then thrown away. Full Health() remains for /health and /metrics.
func (s *RouterService) healthFacts() ProxyHealth {
	s.breakerMu.RLock()
	now := time.Now()
	openCount := 0
	for _, state := range s.modelBreakers {
		if state.OpenUntil.After(now) {
			openCount++
		}
	}
	anyAvailable := false
	for _, attempt := range s.allKnownAttempts() {
		if s.isModelAvailableLocked(attempt) {
			anyAvailable = true
			break
		}
	}
	s.breakerMu.RUnlock()

	status := "ok"
	ready := true
	if openCount > 0 {
		status = "degraded"
	}
	if !anyAvailable {
		ready, status = false, "unavailable"
	}
	if strict, warnings := s.strictAndWarnings(); strict && len(warnings) > 0 {
		ready, status = false, "unavailable"
	}
	return ProxyHealth{Status: status, Ready: ready, OpenCircuitCount: openCount}
}

// ambientKeyConfigured reports whether an upstream key is available from any
// ambient source. The binary injects AmbientKeyPresent so a key stored by
// `calvoproxy login` counts too; without it we can only see the env var.
func (s *RouterService) ambientKeyConfigured() bool {
	if s.AmbientKeyPresent != nil {
		return s.AmbientKeyPresent()
	}
	return envValue("OPENROUTER_API_KEY") != ""
}

// Health returns a ProxyHealth snapshot of all circuit breakers.
func (s *RouterService) Health() ProxyHealth {
	s.breakerMu.RLock()
	defer s.breakerMu.RUnlock()

	snapshots := make([]BreakerSnapshot, 0, len(s.modelBreakers))
	openCount := 0
	env := s.scoreEnv()
	now := env.now
	for model, state := range s.modelBreakers {
		snapshot := BreakerSnapshot{
			Model:               model,
			ConsecutiveFailures: state.ConsecutiveFailures,
			Successes:           state.Successes,
			LastFailureCode:     state.LastFailureCode,
			LastFailureReason:   state.LastFailureReason,
			LastFailureAt:       state.LastFailureAt,
			OpenUntil:           state.OpenUntil,
			Score:               decayedScore(state, env),
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

	// Budget is reported beside health, never mixed into it: an exhausted window
	// is not a degraded circuit, and Status must not say "unavailable" because
	// the day's allowance ran out.
	quotas := s.quota.observe()
	sort.Slice(quotas, func(i, j int) bool { return quotas[i].Scope < quotas[j].Scope })

	status := "ok"
	ready := true
	if openCount > 0 {
		status = "degraded"
	}
	// Compute readiness under the read lock we already hold, using the lock-free
	// availability check — NOT filterAvailableAttempts, whose isModelAvailable
	// would take a second (re-entrant) RLock and deadlock if a writer is waiting.
	anyAvailable := false
	for _, attempt := range s.allKnownAttempts() {
		if s.isModelAvailableLocked(attempt) {
			anyAvailable = true
			break
		}
	}
	if !anyAvailable {
		ready = false
		status = "unavailable"
	}
	// Read the reload-managed validation state under policyMu (lock order stays
	// breakerMu → policyMu, matching getPolicy() calls elsewhere in this method).
	if strict, warnings := s.strictAndWarnings(); strict && len(warnings) > 0 {
		ready = false
		status = "unavailable"
	}

	return ProxyHealth{
		Service:            "CalvoProxy",
		Status:             status,
		Ready:              ready,
		OpenCircuitCount:   openCount,
		ConfiguredAPIKey:   s.ambientKeyConfigured(),
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
		Quotas:             quotas,
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
	// probeUntil single-flights the half-open recovery probe, mirroring the
	// per-model breaker: after the cooldown elapses exactly one request probes
	// the host instead of every concurrent caller stampeding it.
	probeUntil time.Time
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
	now := time.Now()
	switch {
	case now.Before(hb.openUntil):
		t.mu.Unlock()
		return nil, fmt.Errorf("circuit breaker open for host %s: temporarily unreachable", host)
	case !hb.openUntil.IsZero():
		// Half-open: cooldown elapsed. Let exactly one probe through; everyone
		// else keeps failing fast until it resolves or the probe TTL lapses.
		if now.Before(hb.probeUntil) {
			t.mu.Unlock()
			return nil, fmt.Errorf("circuit breaker recovering for host %s: probe in flight", host)
		}
		// The probe TTL must outlast a slow probe request, or it lapses while the
		// probe is still in flight and a second one stampedes the recovering host.
		ttl := t.Cooldown
		if hdr := 60 * time.Second; ttl < hdr {
			ttl = hdr
		}
		hb.probeUntil = now.Add(ttl)
	}
	t.mu.Unlock()

	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	resp, err := base.RoundTrip(req)

	t.mu.Lock()
	defer t.mu.Unlock()
	hb = t.hostState(host)
	hb.probeUntil = time.Time{} // this attempt resolved any in-flight probe

	if err != nil {
		// A cancelled request says nothing about the host. The client hung up,
		// or we abandoned the attempt — the host may be perfectly healthy and
		// was simply never given time to answer. Counting it here is worse than
		// at the model level: this circuit gates EVERY model on the host, so a
		// handful of impatient clients would take out all of openrouter.ai.
		//
		// Neutral, exactly like the 429 case below: neither a failure nor a
		// success, so it cannot erase real accumulated host faults either.
		if errors.Is(err, context.Canceled) {
			return resp, err
		}
		hb.failures++
		if hb.failures >= t.FailureThreshold {
			hb.openUntil = time.Now().Add(t.Cooldown)
			slog.Error("[CalvoProxy] 🚨 HOST CIRCUIT OPEN: host is down", slog.String("host", host), slog.String("open_until", hb.openUntil.Format(time.RFC3339)))
		}
		return resp, err
	}

	switch {
	case resp.StatusCode == http.StatusBadGateway ||
		resp.StatusCode == http.StatusServiceUnavailable ||
		resp.StatusCode == http.StatusGatewayTimeout:
		// Genuine host-level faults.
		hb.failures++
		if hb.failures >= t.FailureThreshold {
			hb.openUntil = time.Now().Add(t.Cooldown)
			slog.Error("[CalvoProxy] 🚨 HOST CIRCUIT OPEN: host is returning errors", slog.String("host", host), slog.Int("http_code", resp.StatusCode), slog.String("open_until", hb.openUntil.Format(time.RFC3339)))
		}
	case resp.StatusCode == http.StatusTooManyRequests:
		// Rate limiting is NEUTRAL at the host level: it is a per-model/per-key
		// quota signal, already handled by the model breaker (with Retry-After).
		// Counting it here would open the circuit for EVERY model on the host;
		// treating it as success would erase real accumulated host failures.
	case resp.StatusCode < 500:
		// A real answer from the host: it is healthy again.
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
