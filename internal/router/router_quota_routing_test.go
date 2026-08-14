package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type quota429Transport struct{}

func (quota429Transport) RoundTrip(*http.Request) (*http.Response, error) {
	headers := make(http.Header)
	headers.Set("X-RateLimit-Limit-Tokens-Minute", "64000")
	headers.Set("X-RateLimit-Remaining-Tokens-Minute", "0")
	headers.Set("X-RateLimit-Reset-Tokens-Minute", "20s")
	headers.Set("Retry-After", "20")
	return &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers, Body: io.NopCloser(strings.NewReader(`{"error":"quota"}`))}, nil
}

func TestQuota429DoesNotChangeReliabilityOrOpenProvider(t *testing.T) {
	t.Setenv("CEREBRAS_API_KEY", "cerebras-secret")
	s := NewRouterService()
	t.Cleanup(s.Close)
	s.Client = &http.Client{Transport: quota429Transport{}}
	attempt := modelAttempt{Profile: "coding", Provider: providerCerebras, Model: "gpt-oss-120b", BreakerPolicy: BreakerPolicy{Eligible: true}}
	now := time.Now()
	s.modelBreakers[s.breakerKey(attempt)] = &modelBreakerState{Score: .83, ScoreUpdatedAt: now, ScoreAttemptSeq: 7, Successes: 4}
	s.scoreAttempts.Store(7)
	ctx := WithAmbientProviderCredentials(context.Background(), true)
	err := s.executeAttempt(ctx, httptest.NewRecorder(), []byte(`{"messages":[{"role":"user","content":"hi"}],"max_completion_tokens":100}`), "", attempt)
	attErr, ok := err.(*attemptError)
	if !ok || !attErr.QuotaLimited || !attErr.SkipModel {
		t.Fatalf("expected model-local quota failure, got %#v", err)
	}
	state := s.modelBreakers[s.breakerKey(attempt)]
	if state.Score != .83 || state.Successes != 4 || state.ScoreAttemptSeq != 7 || s.scoreAttempts.Load() != 7 {
		t.Fatalf("quota changed reliability evidence: state=%+v seq=%d", state, s.scoreAttempts.Load())
	}
	if provider := s.providerBreakerSnapshot(providerCerebras); provider.State != "closed" || provider.ConsecutiveFailures != 0 {
		t.Fatalf("model token quota opened provider breaker: %+v", provider)
	}
	quota := s.quotaHealth(providerCerebras, time.Now())
	if len(quota) == 0 || quota[len(quota)-1].Remaining != 0 {
		t.Fatalf("quota headers were not recorded: %+v", quota)
	}
}

type authFailureTransport struct{}

func (authFailureTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"bad credential"}`))}, nil
}

func TestDirectProviderAuthFailureDoesNotChangeModelReliability(t *testing.T) {
	t.Setenv("CEREBRAS_API_KEY", "expired-secret")
	s := NewRouterService()
	t.Cleanup(s.Close)
	s.Client = &http.Client{Transport: authFailureTransport{}}
	attempt := modelAttempt{Profile: "coding", Provider: providerCerebras, Model: "gpt-oss-120b", BreakerPolicy: BreakerPolicy{Eligible: true}}
	now := time.Now()
	s.modelBreakers[s.breakerKey(attempt)] = &modelBreakerState{Score: .71, ScoreUpdatedAt: now, ScoreAttemptSeq: 4, Successes: 2}
	s.scoreAttempts.Store(4)
	ctx := WithAmbientProviderCredentials(context.Background(), true)
	_ = s.executeAttempt(ctx, httptest.NewRecorder(), []byte(`{"messages":[]}`), "", attempt)
	state := s.modelBreakers[s.breakerKey(attempt)]
	if state.Score != .71 || state.ScoreAttemptSeq != 4 || state.Successes != 2 || s.scoreAttempts.Load() != 4 {
		t.Fatalf("credential failure contaminated model reliability: %+v seq=%d", state, s.scoreAttempts.Load())
	}
	if provider := s.providerBreakerSnapshot(providerCerebras); provider.State != "open" {
		t.Fatalf("direct credential failure should open provider auth circuit: %+v", provider)
	}
}

func TestQuotaCredentialRotationDoesNotInheritOldExhaustion(t *testing.T) {
	s := &RouterService{modelBreakers: map[string]*modelBreakerState{}, providerAttempts: map[providerID]int64{}, providerBalance: map[providerID]int64{}, quota: NewQuotaLedger()}
	attempt := modelAttempt{Profile: "coding", Provider: providerGroq, Model: "openai/gpt-oss-120b"}
	now := time.Now()
	oldScope := credentialQuotaScope("model", "old-key")
	oldKey := QuotaBucketKey{Provider: string(providerGroq), Scope: oldScope, ModelOrPool: attempt.Model, Dimension: QuotaDimensionRequests, Window: QuotaWindowDay}
	s.quota.Observe(oldKey, QuotaObservation{Limit: 1000, Remaining: 0, ResetAt: now.Add(time.Hour), Source: QuotaSourceProviderHeader, Confidence: QuotaConfidenceAuthoritative}, now)
	reservations := s.quotaReservations(attempt, "new-key", QuotaEstimate{Requests: 1, Tokens: 1}, now)
	if len(reservations) == 0 {
		t.Fatal("new credential did not receive bootstrap buckets")
	}
	for _, reservation := range reservations {
		if reservation.Key.Scope == oldScope {
			t.Fatalf("new credential touched old exhausted bucket: %+v", reservation)
		}
	}
	ticket, ok := s.quota.ReserveAll(reservations, now)
	if !ok || !ticket.Valid() {
		t.Fatal("old credential exhaustion blocked the rotated key")
	}
}

func TestQuotaHealthDoesNotExposeCredentialFingerprint(t *testing.T) {
	s := &RouterService{quota: NewQuotaLedger()}
	now := time.Now()
	secret := "health-secret-key"
	internalScope := credentialQuotaScope("model", secret)
	key := QuotaBucketKey{Provider: string(providerGroq), Scope: internalScope, ModelOrPool: "m", Dimension: QuotaDimensionRequests, Window: QuotaWindowDay}
	s.quota.Observe(key, QuotaObservation{Limit: 10, Remaining: 9, ResetAt: now.Add(time.Hour), Source: QuotaSourceProviderHeader, Confidence: QuotaConfidenceAuthoritative}, now)
	encoded, _ := json.Marshal(s.quotaHealth(providerGroq, now))
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), strings.TrimPrefix(internalScope, "model:")) {
		t.Fatalf("health leaked credential identity: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"scope":"model"`) {
		t.Fatalf("health lost public quota scope: %s", encoded)
	}
}

func TestQuotaPressureBandUsesReliabilityThenLatency(t *testing.T) {
	t.Setenv("CEREBRAS_API_KEY", "cb")
	ctx := WithAmbientProviderCredentials(context.Background(), true)
	s := &RouterService{modelBreakers: map[string]*modelBreakerState{}, providerAttempts: map[providerID]int64{}, providerBalance: map[providerID]int64{}, quota: NewQuotaLedger(), quotaCooldowns: map[string]time.Time{}}
	now := time.Now()
	or := modelAttempt{Profile: "coding", Provider: providerOpenRouter, Model: "or:free"}
	cb := modelAttempt{Profile: "coding", Provider: providerCerebras, Model: "gpt-oss-120b"}
	s.modelBreakers[s.breakerKey(or)] = &modelBreakerState{Score: .2, ScoreUpdatedAt: now, Successes: 1}
	s.modelBreakers[s.breakerKey(cb)] = &modelBreakerState{Score: .9, ScoreUpdatedAt: now, Successes: 1}
	s.quota.Observe(QuotaBucketKey{Provider: string(providerOpenRouter), Scope: credentialQuotaScope("free_pool", "or-key"), ModelOrPool: quotaFreePool, Dimension: QuotaDimensionRequests, Window: QuotaWindowDay}, QuotaObservation{Limit: 100, Remaining: 90, ResetAt: now.Add(time.Hour), Source: QuotaSourceProviderHeader, Confidence: QuotaConfidenceAuthoritative}, now)
	s.quota.Observe(QuotaBucketKey{Provider: string(providerCerebras), Scope: credentialQuotaScope("model", "cb"), ModelOrPool: cb.Model, Dimension: QuotaDimensionRequests, Window: QuotaWindowDay}, QuotaObservation{Limit: 100, Remaining: 89, ResetAt: now.Add(time.Hour), Source: QuotaSourceProviderHeader, Confidence: QuotaConfidenceAuthoritative}, now)
	ranked := s.quotaRankAndReserve(ctx, []modelAttempt{or, cb}, "or-key", QuotaEstimate{Requests: 1})
	if len(ranked) == 0 || ranked[0].Provider != providerCerebras {
		t.Fatalf("1pp pressure difference should stay in band and prefer reliability: %+v", ranked)
	}
	s.quota.Release(ranked[0].QuotaTicket, now)

	// Equal reliability/pressure: established TTFT beyond the latency budget is
	// demoted, so fairness cannot degrade interactive response speed.
	s.modelBreakers[s.breakerKey(or)].Score = .9
	for i := 0; i < 3; i++ {
		s.recordFirstTokenLatency(or, 3*time.Second)
		s.recordFirstTokenLatency(cb, 200*time.Millisecond)
	}
	ranked = s.quotaRankAndReserve(ctx, []modelAttempt{or, cb}, "or-key", QuotaEstimate{Requests: 1})
	if len(ranked) == 0 || ranked[0].Provider != providerCerebras {
		t.Fatalf("latency guard did not demote established slow target: %+v", ranked)
	}
}

func TestQuotaRankingInterleavesProvidersAndRemovesKnownIneligibleTail(t *testing.T) {
	t.Setenv("CEREBRAS_API_KEY", "cb")
	t.Setenv("GROQ_API_KEY", "groq")
	ctx := WithAmbientProviderCredentials(context.Background(), true)
	s := &RouterService{modelBreakers: map[string]*modelBreakerState{}, providerAttempts: map[providerID]int64{}, providerBalance: map[providerID]int64{}, quota: NewQuotaLedger(), quotaCooldowns: map[string]time.Time{}}
	attempts := []modelAttempt{
		{Profile: "coding", Provider: providerOpenRouter, Model: "or-1:free"},
		{Profile: "coding", Provider: providerOpenRouter, Model: "or-2:free"},
		{Profile: "coding", Provider: providerCerebras, Model: "gpt-oss-120b"},
		{Profile: "coding", Provider: providerGroq, Model: "openai/gpt-oss-120b"},
	}
	ranked := s.quotaRankAndReserve(ctx, attempts, "or", QuotaEstimate{Requests: 1, Tokens: 100})
	if len(ranked) < 2 || ranked[0].Provider == ranked[1].Provider {
		t.Fatalf("MaxAttempts=2 would erase provider diversity: %+v", ranked)
	}
	s.quota.Release(ranked[0].QuotaTicket, time.Now())

	now := time.Now()
	orKey := QuotaBucketKey{Provider: string(providerOpenRouter), Scope: credentialQuotaScope("free_pool", "or"), ModelOrPool: quotaFreePool, Dimension: QuotaDimensionRequests, Window: QuotaWindowDay}
	s.quota.Observe(orKey, QuotaObservation{Limit: 50, Remaining: 0, ResetAt: now.Add(time.Hour), Source: QuotaSourceProviderHeader, Confidence: QuotaConfidenceAuthoritative}, now)
	ranked = s.quotaRankAndReserve(ctx, []modelAttempt{attempts[2], attempts[0]}, "or", QuotaEstimate{Requests: 1, Tokens: 100})
	if len(ranked) != 1 || ranked[0].Provider != providerCerebras {
		t.Fatalf("known-ineligible trailing fallback must be removed before LastInChain: %+v", ranked)
	}
}

func TestProviderSpecificEstimateAndEveryQuotaDimensionGate(t *testing.T) {
	t.Setenv("CEREBRAS_API_KEY", "cb")
	s := &RouterService{modelBreakers: map[string]*modelBreakerState{}, providerAttempts: map[providerID]int64{}, providerBalance: map[providerID]int64{}, quota: NewQuotaLedger(), quotaCooldowns: map[string]time.Time{}}
	attempt := modelAttempt{Profile: "coding", Provider: providerCerebras, Model: "gpt-oss-120b"}
	estimate := estimateRequestQuota(map[string]interface{}{"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}}})
	adjusted := providerQuotaEstimate(attempt, estimate)
	if adjusted.OutputExplicit || adjusted.OutputTokens != 32768 || adjusted.Tokens <= estimate.Tokens {
		t.Fatalf("Cerebras omitted max_completion_tokens was under-reserved: generic=%+v adjusted=%+v", estimate, adjusted)
	}
	now := time.Now()
	s.ensureBootstrapQuota(attempt, "cb", now)
	scope := credentialQuotaScope("model", "cb")
	rpm := QuotaBucketKey{Provider: string(providerCerebras), Scope: scope, ModelOrPool: attempt.Model, Dimension: QuotaDimensionRequests, Window: QuotaWindowMinute}
	s.quota.Observe(rpm, QuotaObservation{Limit: 30, Remaining: 0, ResetAt: now.Add(time.Minute), Source: QuotaSourceProviderHeader, Confidence: QuotaConfidenceAuthoritative}, now)
	if s.quotaCanFit(attempt, "cb", QuotaEstimate{Requests: 1, Tokens: 1}, now) {
		t.Fatal("zero RPM must reject despite available daily quota")
	}
	s.quota.Observe(rpm, QuotaObservation{Limit: 30, Remaining: 30, ResetAt: now.Add(time.Minute), Source: QuotaSourceProviderHeader, Confidence: QuotaConfidenceAuthoritative}, now)
	tpd := QuotaBucketKey{Provider: string(providerCerebras), Scope: scope, ModelOrPool: attempt.Model, Dimension: QuotaDimensionTokens, Window: QuotaWindowDay}
	s.quota.Observe(tpd, QuotaObservation{Limit: 1_000_000, Remaining: 0, ResetAt: now.Add(time.Hour), Source: QuotaSourceProviderHeader, Confidence: QuotaConfidenceAuthoritative}, now)
	if s.quotaCanFit(attempt, "cb", QuotaEstimate{Requests: 1, Tokens: 1}, now) {
		t.Fatal("zero TPD must reject despite available minute request quota")
	}
}

type countingDailyQuotaTransport struct{ calls int }

func (t *countingDailyQuotaTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	reset := time.Now().Add(time.Hour).UnixMilli()
	body := `{"error":{"code":429,"metadata":{"limit_source":"openrouter_free_tier_daily","headers":{"X-RateLimit-Limit":"50","X-RateLimit-Remaining":"0","X-RateLimit-Reset":"` + strconv.FormatInt(reset, 10) + `"}}}}`
	return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"3600"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestSecondRequestUsesPersistedQuotaForEnglishRetryResponseWithoutUpstreamCall(t *testing.T) {
	transport := &countingDailyQuotaTransport{}
	svc := newTestService(t, &http.Client{Transport: transport}, policyConfig{
		DefaultProfile: "coding",
		Profiles:       map[string][]string{"coding": {"only:free"}},
		Aliases:        map[string]string{"coding": "coding"},
	})
	for requestNumber := 1; requestNumber <= 2; requestNumber++ {
		req := trustedRequest(http.MethodPost, "/v1/chat/completions", `{"model":"coding","messages":[{"role":"user","content":"hi"}]}`)
		rec := newHeaderSnapshotRecorder()
		svc.RouteRequest(rec, req, "openrouter-secret")
		if requestNumber == 2 {
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
				t.Fatalf("second request needs 503 + Retry-After, got %d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "temporarily rate-limited") || !strings.Contains(rec.Body.String(), "request was not sent upstream") {
				t.Fatalf("second request lacks actionable English explanation: %s", rec.Body.String())
			}
		}
	}
	if transport.calls != 1 {
		t.Fatalf("known exhausted quota should prevent a second upstream call, got %d", transport.calls)
	}
}

type countingBareQuotaTransport struct{ calls int }

func (t *countingBareQuotaTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"error":"rate limited"}`)),
	}, nil
}

func TestBare429PublishesDefaultRetryAfterAndBlocksImmediateResend(t *testing.T) {
	// This test verifies the fail-fast refusal itself, so the in-proxy hold
	// (which would otherwise ride out the 2s cooldown and resend) is disabled.
	t.Setenv("PROXY_QUOTA_HOLD_MAX_MS", "0")
	transport := &countingBareQuotaTransport{}
	svc := newTestService(t, &http.Client{Transport: transport}, policyConfig{
		DefaultProfile: "coding",
		Profiles:       map[string][]string{"coding": {"only:free"}},
		Aliases:        map[string]string{"coding": "coding"},
	})

	first := newHeaderSnapshotRecorder()
	svc.RouteRequest(first, trustedRequest(http.MethodPost, "/v1/chat/completions", `{"model":"coding","messages":[{"role":"user","content":"hi"}]}`), "openrouter-secret")
	if first.Header().Get("Retry-After") == "" {
		t.Fatalf("bare 429 must expose the local cooldown through Retry-After: headers=%v body=%s", first.Header(), first.Body.String())
	}

	second := newHeaderSnapshotRecorder()
	svc.RouteRequest(second, trustedRequest(http.MethodPost, "/v1/chat/completions", `{"model":"coding","messages":[{"role":"user","content":"hi"}]}`), "openrouter-secret")
	if second.Code != http.StatusServiceUnavailable || second.Header().Get("Retry-After") == "" {
		t.Fatalf("local cooldown needs 503 + Retry-After, got %d headers=%v body=%s", second.Code, second.Header(), second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "temporarily rate-limited") || !strings.Contains(second.Body.String(), "request was not sent upstream") {
		t.Fatalf("local cooldown lacks actionable English explanation: %s", second.Body.String())
	}
	if transport.calls != 1 {
		t.Fatalf("cooldown should prevent the immediate resend, got %d upstream calls", transport.calls)
	}
}

// recoveringQuotaTransport fails the first call with a bare 429 (which installs
// the default local cooldown) and serves the second call, so a held request can
// prove it recovered without any client-side retry.
type recoveringQuotaTransport struct{ calls int }

func (t *recoveringQuotaTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	if t.calls == 1 {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"error":"rate limited"}`)),
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`)),
	}, nil
}

func TestQuotaHoldRidesOutCooldownAndRecoversWithoutClientRetry(t *testing.T) {
	t.Setenv("PROXY_QUOTA_HOLD_MAX_MS", "10000")
	transport := &recoveringQuotaTransport{}
	svc := newTestService(t, &http.Client{Transport: transport}, policyConfig{
		DefaultProfile: "coding",
		Profiles:       map[string][]string{"coding": {"only:free"}},
		Aliases:        map[string]string{"coding": "coding"},
	})

	first := newHeaderSnapshotRecorder()
	svc.RouteRequest(first, trustedRequest(http.MethodPost, "/v1/chat/completions", `{"model":"coding","messages":[{"role":"user","content":"hi"}]}`), "openrouter-secret")
	if transport.calls != 1 {
		t.Fatalf("first request should reach upstream exactly once, got %d", transport.calls)
	}

	// The bare 429 installed a ~2s local cooldown. The second request must be
	// parked in-proxy past that cooldown and then succeed on its own.
	start := time.Now()
	second := newHeaderSnapshotRecorder()
	svc.RouteRequest(second, trustedRequest(http.MethodPost, "/v1/chat/completions", `{"model":"coding","messages":[{"role":"user","content":"hi"}]}`), "openrouter-secret")
	if second.Code != http.StatusOK {
		t.Fatalf("held request should recover with 200, got %d body=%s", second.Code, second.Body.String())
	}
	if transport.calls != 2 {
		t.Fatalf("recovery needs exactly one more upstream call, got %d", transport.calls)
	}
	if waited := time.Since(start); waited < time.Second {
		t.Fatalf("second request did not actually wait out the cooldown (took %v)", waited)
	}
	if held := svc.Counters().QuotaHeld; held < 1 {
		t.Fatalf("hold was not counted: QuotaHeld=%d", held)
	}
}

// TestQuotaHoldSkipsWhenBreakerNotQuotaIsTheBlocker guards against holding for
// a quota reset that cannot possibly help: the only configured model is
// breaker-open (unhealthy), so nothing survives even the breaker/context gate
// before quota is ever consulted. If the hold decision were computed over the
// full planned chain (attemptsToTry) instead of the breaker-healthy subset, a
// stale quota observation on that same excluded model would still report a
// positive wait and park the request for it — burning most of the hold budget
// only to hit the same breaker-open 503 afterwards.
func TestQuotaHoldSkipsWhenBreakerNotQuotaIsTheBlocker(t *testing.T) {
	t.Setenv("PROXY_QUOTA_HOLD_MAX_MS", "10000")
	transport := &countingBareQuotaTransport{}
	svc := newTestService(t, &http.Client{Transport: transport}, policyConfig{
		DefaultProfile: "coding",
		// A second, healthy profile keeps the proxy's overall readiness gate
		// (healthFacts: "is ANY known model available") open, so the request
		// actually reaches dispatchChain's hold decision instead of being
		// rejected upstream for global unavailability — this test targets the
		// per-chain breaker-vs-quota logic, not the readiness gate.
		Profiles: map[string][]string{"coding": {"only:free"}, "simple": {"healthy:free"}},
		Aliases:  map[string]string{"coding": "coding", "simple": "simple"},
	})
	attempt := modelAttempt{Profile: "coding", Provider: providerOpenRouter, Model: "only:free"}
	now := time.Now()
	svc.modelBreakers[svc.breakerKey(attempt)] = &modelBreakerState{OpenUntil: now.Add(30 * time.Second)}
	// A quota observation whose reset comfortably fits inside holdBudget, so
	// quotaRetryAfterForAttempts over the FULL chain reports a positive
	// (bogus) wait for this already-breaker-excluded model that a buggy
	// implementation would happily wait out.
	svc.quotaLedger().Observe(
		QuotaBucketKey{Provider: string(providerOpenRouter), Scope: credentialQuotaScope("free_pool", "openrouter-secret"), ModelOrPool: quotaFreePool, Dimension: QuotaDimensionRequests, Window: QuotaWindowDay},
		QuotaObservation{Limit: 50, Remaining: 0, ResetAt: now.Add(3 * time.Second), Source: QuotaSourceProviderHeader, Confidence: QuotaConfidenceAuthoritative},
		now,
	)

	start := time.Now()
	rec := newHeaderSnapshotRecorder()
	svc.RouteRequest(rec, trustedRequest(http.MethodPost, "/v1/chat/completions", `{"model":"coding","messages":[{"role":"user","content":"hi"}]}`), "openrouter-secret")
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("breaker-open request should fail fast with 503, got %d body=%s", rec.Code, rec.Body.String())
	}
	if elapsed > time.Second {
		t.Fatalf("breaker-open request should fail immediately, not hold for an unrelated quota reset: took %v", elapsed)
	}
	if transport.calls != 0 {
		t.Fatalf("breaker-open model must never be dialed, got %d calls", transport.calls)
	}
	if held := svc.Counters().QuotaHeld; held != 0 {
		t.Fatalf("must not count a hold when breaker, not quota, is the blocker: QuotaHeld=%d", held)
	}
}
