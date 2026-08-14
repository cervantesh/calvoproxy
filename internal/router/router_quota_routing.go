package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

const quotaFreePool = "free"

// quotaHoldSettle pads every in-proxy hold past the computed reset instant:
// provider windows are second-granular and clocks disagree slightly, so waking
// exactly at the reset would re-read a still-empty bucket about half the time.
const quotaHoldSettle = 150 * time.Millisecond

// quotaHoldBudget is the total wall-clock a single request may spend parked
// inside the proxy waiting for quota/cooldown resets, instead of immediately
// returning the 503 + Retry-After it previously returned. The common blocker
// for a large agent request is a minute-window token bucket, so the default
// covers one full minute-window rollover with margin. Set to 0 to disable
// holding and restore fail-fast behaviour.
func quotaHoldBudget() time.Duration {
	return time.Duration(envInt("PROXY_QUOTA_HOLD_MAX_MS", 75_000)) * time.Millisecond
}

// holdForQuotaReset parks the request until the earliest known quota/cooldown
// reset, so a client that hits a full window recovers by itself instead of
// being told to retry. Returns false when the hold must not happen: the request
// deadline cannot absorb the wait (waiting would burn the whole budget and
// still fail), or the client went away while parked.
func (s *RouterService) holdForQuotaReset(ctx context.Context, wait time.Duration) bool {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait+quotaHoldSettle+2*time.Second {
		return false
	}
	s.counters.quotaHeld.Add(1)
	slog.InfoContext(ctx, "[CalvoProxy] ⏳ All providers quota-blocked for this request; holding until the earliest reset",
		slog.Duration("wait", wait+quotaHoldSettle))
	timer := time.NewTimer(wait + quotaHoldSettle)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// credentialQuotaScope prevents one API key/account from inheriting another
// key's exhausted bucket after rotation. Only a short one-way fingerprint is
// retained internally; health output strips it.
func credentialQuotaScope(scope, credential string) string {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return scope
	}
	sum := sha256.Sum256([]byte(credential))
	return scope + ":" + hex.EncodeToString(sum[:6])
}

func publicQuotaScope(scope string) string {
	if at := strings.IndexByte(scope, ':'); at >= 0 {
		return scope[:at]
	}
	return scope
}

func (s *RouterService) quotaLedger() *QuotaLedger {
	s.quotaInitMu.Lock()
	defer s.quotaInitMu.Unlock()
	if s.quota == nil {
		// Tests often construct RouterService literals. Lazily creating the
		// ledger keeps those callers compatible without weakening production.
		s.quota = NewQuotaLedger()
	}
	return s.quota
}

func estimateRequestQuota(body map[string]interface{}) QuotaEstimate {
	encoded, _ := json.Marshal(body)
	// A tokenizer-independent conservative approximation. JSON punctuation and
	// ASCII text average near four bytes/token; rounding up avoids zero-cost
	// prompts. max_completion_tokens/max_tokens is provider-reserved capacity.
	input := int64(math.Ceil(float64(len(encoded)) / 4.0))
	if input < 1 {
		input = 1
	}
	output := int64(envInt("PROXY_DEFAULT_OUTPUT_TOKENS", 1024))
	explicit := false
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		if value, ok := numericInt64(body[key]); ok && value >= 0 {
			output = value
			explicit = true
			break
		}
	}
	return QuotaEstimate{Requests: 1, Tokens: input + output, InputTokens: input, OutputTokens: output, OutputExplicit: explicit}
}

func providerQuotaEstimate(attempt modelAttempt, estimate QuotaEstimate) QuotaEstimate {
	if estimate.OutputExplicit || estimate.InputTokens <= 0 {
		return estimate
	}
	output := estimate.OutputTokens
	switch attempt.Provider {
	case providerCerebras:
		output = int64(envInt("PROXY_CEREBRAS_DEFAULT_MAX_COMPLETION_TOKENS", 32768))
	case providerGroq:
		output = int64(envInt("PROXY_GROQ_DEFAULT_MAX_COMPLETION_TOKENS", 4096))
	}
	if output < 0 {
		output = 0
	}
	estimate.OutputTokens = output
	estimate.Tokens = estimate.InputTokens + output
	return estimate
}

func numericInt64(value interface{}) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		if v >= 0 && v <= math.MaxInt64 && math.Trunc(v) == v {
			return int64(v), true
		}
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	}
	return 0, false
}

func actualResponseTokens(body []byte, fallback int64) int64 {
	var response struct {
		Usage struct {
			TotalTokens      int64 `json:"total_tokens"`
			InputTokens      int64 `json:"input_tokens"`
			OutputTokens     int64 `json:"output_tokens"`
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &response) == nil {
		if response.Usage.TotalTokens > 0 {
			return response.Usage.TotalTokens
		}
		if total := response.Usage.InputTokens + response.Usage.OutputTokens; total > 0 {
			return total
		}
		if total := response.Usage.PromptTokens + response.Usage.CompletionTokens; total > 0 {
			return total
		}
	}
	return fallback
}

func nextUTCReset(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
}

func nextMinuteReset(now time.Time) time.Time { return now.Truncate(time.Minute).Add(time.Minute) }

// ensureBootstrapQuota installs conservative published free-tier defaults only
// until a provider header/body supplies account-specific authoritative values.
// Every value is overrideable because provider/account limits change.
func (s *RouterService) ensureBootstrapQuota(attempt modelAttempt, credential string, now time.Time) {
	ledger := s.quotaLedger()
	observeIfUnknown := func(key QuotaBucketKey, limit int64, reset time.Time) {
		if limit <= 0 {
			return
		}
		if _, ok := ledger.Snapshot(key, now); ok {
			return
		}
		ledger.Observe(key, QuotaObservation{
			Limit: limit, Remaining: limit, ResetAt: reset, ObservedAt: now,
			Source: QuotaSourceConfig, Confidence: QuotaConfidenceEstimated,
		}, now)
	}
	provider := string(s.normalizedProvider(attempt))
	switch attempt.Provider {
	case providerOpenRouter:
		if !strings.HasSuffix(strings.ToLower(attempt.Model), ":free") {
			return
		}
		key := QuotaBucketKey{Provider: provider, Scope: credentialQuotaScope("free_pool", credential), ModelOrPool: quotaFreePool, Dimension: QuotaDimensionRequests, Window: QuotaWindowDay}
		observeIfUnknown(key, int64(envInt("PROXY_OPENROUTER_FREE_RPD", 50)), nextUTCReset(now))
		observeIfUnknown(QuotaBucketKey{Provider: provider, Scope: key.Scope, ModelOrPool: quotaFreePool, Dimension: QuotaDimensionRequests, Window: QuotaWindowMinute}, int64(envInt("PROXY_OPENROUTER_FREE_RPM", 20)), nextMinuteReset(now))
	case providerCerebras:
		scope := credentialQuotaScope("model", credential)
		observeIfUnknown(QuotaBucketKey{Provider: provider, Scope: scope, ModelOrPool: attempt.Model, Dimension: QuotaDimensionRequests, Window: QuotaWindowDay}, int64(envInt("PROXY_CEREBRAS_RPD", 14400)), now.Add(24*time.Hour))
		observeIfUnknown(QuotaBucketKey{Provider: provider, Scope: scope, ModelOrPool: attempt.Model, Dimension: QuotaDimensionRequests, Window: QuotaWindowMinute}, int64(envInt("PROXY_CEREBRAS_RPM", 30)), nextMinuteReset(now))
		observeIfUnknown(QuotaBucketKey{Provider: provider, Scope: scope, ModelOrPool: attempt.Model, Dimension: QuotaDimensionTokens, Window: QuotaWindowMinute}, int64(envInt("PROXY_CEREBRAS_TPM", 64000)), nextMinuteReset(now))
		observeIfUnknown(QuotaBucketKey{Provider: provider, Scope: scope, ModelOrPool: attempt.Model, Dimension: QuotaDimensionTokens, Window: QuotaWindowDay}, int64(envInt("PROXY_CEREBRAS_TPD", 1000000)), now.Add(24*time.Hour))
	case providerGroq:
		rpd, tpm, tpd := 1000, 8000, 200000
		if strings.Contains(strings.ToLower(attempt.Model), "llama-3.1-8b") {
			rpd, tpm, tpd = 14400, 6000, 500000
		}
		scope := credentialQuotaScope("model", credential)
		observeIfUnknown(QuotaBucketKey{Provider: provider, Scope: scope, ModelOrPool: attempt.Model, Dimension: QuotaDimensionRequests, Window: QuotaWindowDay}, int64(envInt("PROXY_GROQ_RPD", rpd)), now.Add(24*time.Hour))
		observeIfUnknown(QuotaBucketKey{Provider: provider, Scope: scope, ModelOrPool: attempt.Model, Dimension: QuotaDimensionRequests, Window: QuotaWindowMinute}, int64(envInt("PROXY_GROQ_RPM", 30)), nextMinuteReset(now))
		observeIfUnknown(QuotaBucketKey{Provider: provider, Scope: scope, ModelOrPool: attempt.Model, Dimension: QuotaDimensionTokens, Window: QuotaWindowMinute}, int64(envInt("PROXY_GROQ_TPM", tpm)), nextMinuteReset(now))
		observeIfUnknown(QuotaBucketKey{Provider: provider, Scope: scope, ModelOrPool: attempt.Model, Dimension: QuotaDimensionTokens, Window: QuotaWindowDay}, int64(envInt("PROXY_GROQ_TPD", tpd)), now.Add(24*time.Hour))
	}
}

func (s *RouterService) quotaReservations(attempt modelAttempt, credential string, estimate QuotaEstimate, now time.Time) []QuotaReservation {
	estimate = providerQuotaEstimate(attempt, estimate)
	s.ensureBootstrapQuota(attempt, credential, now)
	provider := string(s.normalizedProvider(attempt))
	baseScope := "model"
	if attempt.Provider == providerOpenRouter && strings.HasSuffix(strings.ToLower(attempt.Model), ":free") {
		baseScope = "free_pool"
	}
	expectedScope := credentialQuotaScope(baseScope, credential)
	keys := s.quotaLedger().Keys(now)
	out := make([]QuotaReservation, 0, 4)
	for _, key := range keys {
		if key.Provider != provider || key.Scope != expectedScope {
			continue
		}
		if key.ModelOrPool != attempt.Model && !(attempt.Provider == providerOpenRouter && strings.HasSuffix(strings.ToLower(attempt.Model), ":free") && key.ModelOrPool == quotaFreePool) {
			continue
		}
		cost := estimate.Requests
		if key.Dimension == QuotaDimensionTokens {
			cost = estimate.Tokens
		}
		if cost > 0 {
			out = append(out, QuotaReservation{Key: key, Cost: cost})
		}
	}
	return out
}

func (s *RouterService) quotaPressure(attempt modelAttempt, credential string, estimate QuotaEstimate, now time.Time) (float64, bool) {
	reservations := s.quotaReservations(attempt, credential, estimate, now)
	if len(reservations) == 0 {
		return 0, false
	}
	pressure := 0.0
	for _, reservation := range reservations {
		snapshot, ok := s.quotaLedger().Snapshot(reservation.Key, now)
		if !ok || snapshot.Limit <= 0 {
			continue
		}
		predictedAvailable := snapshot.Available - reservation.Cost
		if predictedAvailable < 0 {
			return 1, true
		}
		used := snapshot.Limit - predictedAvailable
		candidatePressure := float64(used) / float64(snapshot.Limit)
		if candidatePressure > pressure {
			pressure = candidatePressure
		}
	}
	return pressure, true
}

func (s *RouterService) quotaCanFit(attempt modelAttempt, credential string, estimate QuotaEstimate, now time.Time) bool {
	for _, reservation := range s.quotaReservations(attempt, credential, estimate, now) {
		snapshot, ok := s.quotaLedger().Snapshot(reservation.Key, now)
		if !ok || snapshot.Available >= reservation.Cost {
			continue
		}
		// An estimated bucket is an unconfirmed bootstrap default (see
		// ensureBootstrapQuota): it can only be replaced by an authoritative
		// provider observation once a real request reaches that provider. Hard
		// vetoing on the estimate alone would prevent that request from ever
		// being sent, permanently locking the provider out even if its real
		// limits would have allowed it. Only a confirmed shortfall is a hard gate;
		// an estimated shortfall still yields the worst quotaPressure so it stays
		// last in line behind any provider with headroom.
		if snapshot.Confidence == QuotaConfidenceEstimated {
			continue
		}
		return false
	}
	return true
}

func (s *RouterService) quotaCooldownKey(attempt modelAttempt) string {
	return string(s.normalizedProvider(attempt)) + "\x00" + attempt.Model
}

func (s *RouterService) quotaAttemptAvailable(attempt modelAttempt, now time.Time) bool {
	s.quotaCooldownMu.RLock()
	until := s.quotaCooldowns[s.quotaCooldownKey(attempt)]
	s.quotaCooldownMu.RUnlock()
	return !until.After(now)
}

func (s *RouterService) setQuotaCooldown(attempt modelAttempt, duration time.Duration, now time.Time) time.Duration {
	if duration <= 0 {
		duration = 2 * time.Second
	}
	s.quotaCooldownMu.Lock()
	if s.quotaCooldowns == nil {
		s.quotaCooldowns = make(map[string]time.Time)
	}
	key := s.quotaCooldownKey(attempt)
	if until := now.Add(duration); until.After(s.quotaCooldowns[key]) {
		s.quotaCooldowns[key] = until
	}
	s.quotaCooldownMu.Unlock()
	return duration
}

func (s *RouterService) quotaCooldownRemaining(attempt modelAttempt, now time.Time) time.Duration {
	s.quotaCooldownMu.RLock()
	until := s.quotaCooldowns[s.quotaCooldownKey(attempt)]
	s.quotaCooldownMu.RUnlock()
	if !until.After(now) {
		return 0
	}
	return until.Sub(now)
}

// quotaRankAndReserve interleaves provider groups before MaxAttempts truncation,
// orders providers by predicted normalized quota pressure, and atomically
// reserves the primary. Reliability remains the per-provider model tie-break.
func (s *RouterService) quotaRankAndReserve(ctx context.Context, attempts []modelAttempt, openRouterCredential string, estimate QuotaEstimate) []modelAttempt {
	if len(attempts) == 0 {
		return attempts
	}
	now := time.Now()
	policyProviderOrder := make(map[providerID]int, 3)
	for _, attempt := range attempts {
		provider := s.normalizedProvider(attempt)
		if _, exists := policyProviderOrder[provider]; !exists {
			policyProviderOrder[provider] = len(policyProviderOrder)
		}
	}
	// Reliability/quota determine the route for a new session. An existing
	// session may promote its last successful route, but only after the normal
	// breaker/context/configuration gates and only while quota can still fit.
	base := s.applySessionAffinity(ctx, s.rankAttemptsByScore(attempts))
	preferred, hasPreferred := s.affinity.preferred(affinityKeyFrom(ctx))
	if trace := traceFrom(ctx); trace != nil {
		models := make([]string, len(base))
		scores := make([]float64, len(base))
		env := s.scoreEnv()
		for i, attempt := range base {
			models[i] = attempt.Model
			scores[i] = s.scoreForAttempt(attempt, env)
		}
		trace.recordScores(models, scores)
	}
	type group struct {
		provider       providerID
		attempts       []modelAttempt
		pressure       float64
		known          bool
		reliability    float64
		latency        time.Duration
		latencySamples int64
		order          int
		preferred      bool
	}
	groups := make([]group, 0, 3)
	index := map[providerID]int{}
	for _, attempt := range base {
		attempt.Provider = s.normalizedProvider(attempt)
		if !s.quotaAttemptAvailable(attempt, now) {
			continue
		}
		credential, _ := s.providerCredential(ctx, attempt, openRouterCredential)
		if !s.quotaCanFit(attempt, credential, estimate, now) {
			continue
		}
		i, ok := index[attempt.Provider]
		if !ok {
			i = len(groups)
			index[attempt.Provider] = i
			groups = append(groups, group{provider: attempt.Provider, order: policyProviderOrder[attempt.Provider]})
		}
		groups[i].attempts = append(groups[i].attempts, attempt)
		if hasPreferred && attempt.Provider == preferred.Provider && attempt.Model == preferred.Model {
			groups[i].preferred = true
		}
	}
	for i := range groups {
		credential, _ := s.providerCredential(ctx, groups[i].attempts[0], openRouterCredential)
		groups[i].pressure, groups[i].known = s.quotaPressure(groups[i].attempts[0], credential, estimate, now)
		groups[i].reliability = s.scoreForAttempt(groups[i].attempts[0], s.scoreEnv())
		groups[i].latency, groups[i].latencySamples = s.meanFirstTokenLatency(groups[i].attempts[0])
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].preferred != groups[j].preferred {
			return groups[i].preferred
		}
		const pressureBand = 0.05
		if groups[i].known && groups[j].known && math.Abs(groups[i].pressure-groups[j].pressure) > pressureBand {
			return groups[i].pressure < groups[j].pressure
		}
		minSamples := int64(envInt("PROXY_QUOTA_LATENCY_MIN_SAMPLES", 3))
		latencyBudget := time.Duration(envInt("PROXY_QUOTA_LATENCY_BUDGET_MS", 1000)) * time.Millisecond
		if groups[i].latencySamples >= minSamples && groups[j].latencySamples >= minSamples && absDuration(groups[i].latency-groups[j].latency) > latencyBudget {
			return groups[i].latency < groups[j].latency
		}
		if groups[i].reliability != groups[j].reliability {
			return groups[i].reliability > groups[j].reliability
		}
		return groups[i].order < groups[j].order
	})

	// Interleave, so MaxAttempts cannot truncate away every alternate provider.
	ranked := make([]modelAttempt, 0, len(attempts))
	for depth := 0; ; depth++ {
		added := false
		for i := range groups {
			if depth < len(groups[i].attempts) {
				ranked = append(ranked, groups[i].attempts[depth])
				added = true
			}
		}
		if !added {
			break
		}
	}

	// If the least-pressure candidate races with concurrent callers and loses
	// capacity, remove it and try the next candidate without any network wait.
	for len(ranked) > 0 {
		credential, _ := s.providerCredential(ctx, ranked[0], openRouterCredential)
		reservations := s.quotaReservations(ranked[0], credential, estimate, now)
		ticket, ok := s.quotaLedger().ReserveAll(reservations, now)
		if ok {
			ranked[0].QuotaTicket = ticket
			ranked[0].QuotaEstimate = providerQuotaEstimate(ranked[0], estimate)
			return ranked
		}
		ranked = ranked[1:]
	}
	return nil
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func (s *RouterService) meanFirstTokenLatency(attempt modelAttempt) (time.Duration, int64) {
	s.counters.latencyMu.Lock()
	stat := s.counters.firstTokenStats[s.breakerKey(attempt)]
	s.counters.latencyMu.Unlock()
	if stat == nil {
		return 0, 0
	}
	count := stat.count.Load()
	if count <= 0 {
		return 0, 0
	}
	return time.Duration(stat.sumNanos.Load() / count), count
}

func (s *RouterService) reserveFallbackQuota(attempt modelAttempt, credential string, estimate QuotaEstimate, now time.Time) (QuotaTicket, bool) {
	if !s.quotaAttemptAvailable(attempt, now) {
		return QuotaTicket{}, false
	}
	return s.quotaLedger().ReserveAll(s.quotaReservations(attempt, credential, estimate, now), now)
}

// observeAndSettleQuota updates every provider-native bucket and consumes a
// ticket exactly once. When headers do not report usage, actual requests/tokens
// are committed conservatively from the estimate.
func (s *RouterService) observeAndSettleQuota(ticket QuotaTicket, attempt modelAttempt, credential string, status int, headers http.Header, body []byte, actualTokens int64, now time.Time) []providerQuotaObservation {
	observations := observeProviderQuota(attempt.Provider, attempt.Model, status, headers, body, now)
	facts := make(map[QuotaBucketKey]QuotaObservation)
	for _, observation := range observations {
		key, fact, ok := observation.ledgerFact()
		if !ok {
			continue
		}
		key.Scope = credentialQuotaScope(string(observation.Scope), credential)
		if fact.ResetAt.IsZero() && key.Window == QuotaWindowMinute {
			fact.ResetAt = nextMinuteReset(now)
		}
		facts[key] = fact
		s.quotaLedger().Observe(key, fact, now)
	}
	if !ticket.Valid() {
		if len(facts) > 0 {
			s.markScoresDirty()
		}
		return observations
	}
	reservations := s.quotaLedger().Reservations(ticket)
	settlements := make([]QuotaSettlement, 0, len(reservations))
	for _, reservation := range reservations {
		actual := attempt.QuotaEstimate.Requests
		if reservation.Key.Dimension == QuotaDimensionTokens {
			actual = actualTokens
			if actual <= 0 {
				actual = attempt.QuotaEstimate.Tokens
			}
		}
		settlement := QuotaSettlement{Key: reservation.Key, Actual: actual}
		if fact, ok := facts[reservation.Key]; ok {
			copy := fact
			settlement.Observation = &copy
		}
		settlements = append(settlements, settlement)
	}
	s.quotaLedger().Reconcile(ticket, settlements, now)
	s.markScoresDirty()
	return observations
}

func (s *RouterService) quotaHealth(provider providerID, now time.Time) []QuotaHealthSnapshot {
	ledger := s.quotaLedger()
	out := make([]QuotaHealthSnapshot, 0)
	for _, key := range ledger.Keys(now) {
		if key.Provider != string(provider) {
			continue
		}
		snapshot, ok := ledger.Snapshot(key, now)
		if !ok {
			continue
		}
		out = append(out, QuotaHealthSnapshot{
			Scope: publicQuotaScope(key.Scope), ModelOrPool: key.ModelOrPool,
			Dimension: key.Dimension, Window: key.Window,
			Limit: snapshot.Limit, Remaining: snapshot.Remaining,
			Reserved: snapshot.Reserved, Available: snapshot.Available,
			ResetAt: snapshot.ResetAt, Source: snapshot.Source,
			Confidence: snapshot.Confidence,
		})
	}
	return out
}

func (s *RouterService) quotaRetryAfterForAttempts(ctx context.Context, attempts []modelAttempt, openRouterCredential string, estimate QuotaEstimate, now time.Time) time.Duration {
	var soonest time.Duration
	for _, attempt := range attempts {
		credential, _ := s.providerCredential(ctx, attempt, openRouterCredential)
		attemptReadyIn := s.quotaCooldownRemaining(attempt, now)
		for _, reservation := range s.quotaReservations(attempt, credential, estimate, now) {
			snapshot, ok := s.quotaLedger().Snapshot(reservation.Key, now)
			if !ok || snapshot.Available >= reservation.Cost || snapshot.ResetAt.IsZero() || !snapshot.ResetAt.After(now) {
				continue
			}
			wait := snapshot.ResetAt.Sub(now)
			if wait > attemptReadyIn {
				attemptReadyIn = wait
			}
		}
		if attemptReadyIn > 0 && (soonest == 0 || attemptReadyIn < soonest) {
			soonest = attemptReadyIn
		}
	}
	return soonest
}
