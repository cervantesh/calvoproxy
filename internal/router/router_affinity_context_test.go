package router

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestSessionAffinityHeaderPriorityAndCredentialIsolation(t *testing.T) {
	r := &http.Request{Header: http.Header{
		"X-Session-Id":             []string{"generic"},
		"X-Claude-Code-Session-Id": []string{"claude"},
		"X-Calvoproxy-Session-Id":  []string{"explicit"},
	}}
	store := newAffinityStore([]byte("01234567890123456789012345678901"), time.Hour, 10)

	first := store.keyForRequest(r, "credential-a")
	if first == "" {
		t.Fatal("expected affinity key")
	}
	if first == store.key("claude", "credential-a") || first == store.key("generic", "credential-a") {
		t.Fatal("expected CalvoProxy header to have priority")
	}
	if first != store.key("explicit", "credential-a") {
		t.Fatal("expected explicit CalvoProxy session header")
	}
	if first == store.key("explicit", "credential-b") {
		t.Fatal("credentials must isolate identical client session ids")
	}
}

func TestSessionAffinityExpiresAndEvictsLeastRecentlyUsed(t *testing.T) {
	now := time.Unix(100, 0)
	store := newAffinityStore([]byte("01234567890123456789012345678901"), time.Minute, 2)
	store.now = func() time.Time { return now }
	a := store.key("a", "key")
	b := store.key("b", "key")
	c := store.key("c", "key")
	store.pin(a, modelAttempt{Provider: providerOpenRouter, Model: "a"})
	now = now.Add(time.Second)
	store.pin(b, modelAttempt{Provider: providerGroq, Model: "b"})
	if _, ok := store.preferred(a); !ok {
		t.Fatal("expected a to exist")
	}
	now = now.Add(time.Second)
	if _, ok := store.preferred(a); !ok {
		t.Fatal("expected a to remain available")
	}
	now = now.Add(time.Second)
	store.pin(c, modelAttempt{Provider: providerCerebras, Model: "c"})
	if _, ok := store.preferred(b); ok {
		t.Fatal("expected least recently used entry b to be evicted")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := store.preferred(a); ok {
		t.Fatal("expected expired entry")
	}
}

func TestAffinityPromotesEligibleRouteAndSuccessRepins(t *testing.T) {
	s := NewRouterService()
	defer s.Close()
	ctx := withAffinityKey(context.Background(), "session")
	s.affinity.pin("session", modelAttempt{Provider: providerGroq, Model: "preferred"})
	attempts := []modelAttempt{
		{Provider: providerOpenRouter, Model: "normal"},
		{Provider: providerGroq, Model: "preferred"},
	}

	ranked := s.applySessionAffinity(ctx, attempts)
	if ranked[0].Provider != providerGroq || ranked[0].Model != "preferred" {
		t.Fatalf("expected preferred route first, got %+v", ranked)
	}
	s.recordAffinitySuccess(ctx, modelAttempt{Provider: providerCerebras, Model: "fallback"})
	preferred, ok := s.affinity.preferred("session")
	if !ok || preferred.Provider != providerCerebras || preferred.Model != "fallback" {
		t.Fatalf("expected successful fallback to repin session, got %+v %v", preferred, ok)
	}
}

func TestContextFilterUsesProviderModelWindows(t *testing.T) {
	s := &RouterService{contextWindows: contextWindowIndex{
		providerModelKey(providerOpenRouter, "small"): {ContextTokens: 100, OutputReserveTokens: 20},
		providerModelKey(providerGroq, "large"):       {ContextTokens: 300, OutputReserveTokens: 20},
	}}
	attempts := []modelAttempt{
		{Provider: providerOpenRouter, Model: "small"},
		{Provider: providerGroq, Model: "large"},
	}
	estimate := QuotaEstimate{InputTokens: 90, OutputTokens: 20, Tokens: 110, OutputExplicit: true}

	fit, excluded := s.filterContextFit(attempts, estimate)
	if excluded != 1 || len(fit) != 1 || fit[0].Model != "large" {
		t.Fatalf("expected only large model to fit, excluded=%d fit=%+v", excluded, fit)
	}
}

func TestContextFilterKeepsUnknownModelsBackwardCompatible(t *testing.T) {
	s := &RouterService{contextWindows: contextWindowIndex{}}
	attempts := []modelAttempt{{Provider: providerOpenRouter, Model: "custom-model"}}
	fit, excluded := s.filterContextFit(attempts, QuotaEstimate{InputTokens: 1_000_000, OutputTokens: 1})
	if excluded != 0 || len(fit) != 1 {
		t.Fatalf("unknown custom models must remain routable, excluded=%d fit=%+v", excluded, fit)
	}
}

func TestFallbackSuccessCallbackReportsActualSuccessfulRoute(t *testing.T) {
	executor := &fakeAttemptExecutor{errs: []error{
		&attemptError{StatusCode: http.StatusBadGateway, Message: "failed", Retryable: true},
		nil,
	}}
	var succeeded modelAttempt
	err := (DefaultFallbackExecutor{AttemptExecutor: executor}).Execute(context.Background(), nil, FallbackExecution{
		RequestBody: map[string]interface{}{"messages": []interface{}{}},
		Attempts: []modelAttempt{
			{Provider: providerOpenRouter, Model: "failed"},
			{Provider: providerGroq, Model: "worked"},
		},
		RetryPolicy: RetryPolicy{RetryHTTPStatuses: []int{http.StatusBadGateway}},
		OnSuccess:   func(attempt modelAttempt) { succeeded = attempt },
	})
	if err != nil || succeeded.Model != "worked" || succeeded.Provider != providerGroq {
		t.Fatalf("expected successful fallback callback, got err=%v attempt=%+v", err, succeeded)
	}
}

func TestFailedPreferredRouteIsForgottenBeforeFallback(t *testing.T) {
	s := NewRouterService()
	defer s.Close()
	ctx := withAffinityKey(context.Background(), "session")
	failed := modelAttempt{Provider: providerOpenRouter, Model: "failed"}
	s.affinity.pin("session", failed)
	s.recordAffinityFailure(ctx, failed)
	if _, ok := s.affinity.preferred("session"); ok {
		t.Fatal("expected failed preferred route to be forgotten")
	}
}

func TestContextEstimateIsMoreConservativeThanQuotaEstimate(t *testing.T) {
	body := map[string]interface{}{"messages": []interface{}{map[string]interface{}{"role": "user", "content": "abcdefghijklmnopqrstuvwxyz"}}}
	quota := estimateRequestQuota(body)
	contextEstimate := estimateRequestContext(body)
	if contextEstimate.InputTokens <= quota.InputTokens {
		t.Fatalf("expected context estimate %d to exceed quota estimate %d", contextEstimate.InputTokens, quota.InputTokens)
	}
}
