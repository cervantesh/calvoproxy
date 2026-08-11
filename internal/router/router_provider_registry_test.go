package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProviderProfilePlannerUsesExplicitProviderModelPairs(t *testing.T) {
	planner := PolicyModelAttemptPlanner{
		Policy: policyConfig{
			DefaultProfile: "coding",
			Profiles:       map[string][]string{"coding": {"legacy-one", "legacy-two"}},
		},
		ProviderProfiles: providerProfiles{
			"coding": {
				{Provider: providerOpenRouter, Model: "openai/gpt-oss-20b:free"},
				{Provider: providerCerebras, Model: "gpt-oss-120b"},
				{Provider: providerGroq, Model: "openai/gpt-oss-120b"},
			},
		},
	}

	attempts := planner.Plan(allowedTestDecision(providerOpenRouter, Limits{}), "coding", "auto")
	if len(attempts) != 3 {
		t.Fatalf("expected three explicit attempts, got %+v", attempts)
	}
	want := []struct {
		provider string
		model    string
	}{
		{"openrouter", "openai/gpt-oss-20b:free"},
		{"cerebras", "gpt-oss-120b"},
		{"groq", "openai/gpt-oss-120b"},
	}
	for i, attempt := range attempts {
		if string(attempt.Provider) != want[i].provider || attempt.Model != want[i].model {
			t.Errorf("attempt %d = %s/%s; want %s/%s", i, attempt.Provider, attempt.Model, want[i].provider, want[i].model)
		}
	}
}

func TestShippedCodingProfileKeepsOpenRouterFallbackForMessages(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "model-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	profiles, ok := parseProviderProfiles(data)
	if !ok {
		t.Fatal("shipped model policy has no valid provider profiles")
	}
	coding := profiles["coding"]
	foundOpenRouter := false
	for _, target := range coding {
		if target.Provider == providerOpenRouter {
			foundOpenRouter = true
			break
		}
	}
	if !foundOpenRouter {
		t.Fatalf("shipped coding profile must retain an OpenRouter fallback: %+v", coding)
	}

	svc := NewRouterService()
	t.Cleanup(svc.Close)
	attempts := make([]modelAttempt, 0, len(coding))
	for _, target := range coding {
		attempts = append(attempts, modelAttempt{Profile: "coding", Provider: target.Provider, Model: target.Model})
	}
	filtered := svc.filterConfiguredAttempts(context.Background(), attempts, "openrouter-key", messagesPath)
	foundOpenRouter = false
	for _, attempt := range filtered {
		if attempt.Provider == providerOpenRouter {
			foundOpenRouter = true
		}
		if attempt.Provider == providerCerebras || attempt.Provider == providerGroq {
			t.Fatalf("Anthropic messages must reject incompatible direct-provider attempts, got %+v", filtered)
		}
	}
	if !foundOpenRouter {
		t.Fatalf("Anthropic messages must retain the configured OpenRouter coding fallback, got %+v", filtered)
	}
}

func TestProviderCredentialIsolation(t *testing.T) {
	t.Setenv("CEREBRAS_API_KEY", "cerebras-secret")
	t.Setenv("GROQ_API_KEY", "groq-secret")
	ctx := WithAmbientProviderCredentials(context.Background(), true)

	cases := []struct {
		provider providerID
		want     string
	}{
		{providerOpenRouter, "caller-openrouter-key"},
		{providerCerebras, "cerebras-secret"},
		{providerGroq, "groq-secret"},
	}
	for _, tc := range cases {
		got, ok := providerCredential(ctx, modelAttempt{Provider: tc.provider}, "caller-openrouter-key")
		if !ok || got != tc.want {
			t.Errorf("provider %s got credential %q, ok=%v; want its isolated credential", tc.provider, got, ok)
		}
	}

	publicCtx := WithAmbientProviderCredentials(context.Background(), false)
	if got, ok := providerCredential(publicCtx, modelAttempt{Provider: providerCerebras}, "caller-openrouter-key"); ok || got != "" {
		t.Fatalf("ambient Cerebras key escaped onto a disallowed request: %q, ok=%v", got, ok)
	}
	if got, ok := providerCredential(publicCtx, modelAttempt{Provider: providerOllama}, "caller-openrouter-key"); ok || got != "" {
		t.Fatalf("local Ollama escaped onto a disallowed public request: %q, ok=%v", got, ok)
	}
	if got, ok := providerCredential(ctx, modelAttempt{Provider: providerOllama}, "caller-openrouter-key"); !ok || got != "local" {
		t.Fatalf("local Ollama should remain available for an allowed local request: %q, ok=%v", got, ok)
	}
}

func TestProviderCredentialUsesDynamicResolverWithoutCrossProviderLeak(t *testing.T) {
	t.Setenv("CEREBRAS_API_KEY", "")
	t.Setenv("GROQ_API_KEY", "")
	ctx := WithAmbientProviderCredentials(context.Background(), true)
	keys := map[string]string{"cerebras": "vault-cerebras", "groq": "vault-groq"}
	svc := NewRouterService()
	t.Cleanup(svc.Close)
	svc.AmbientProviderCredential = func(provider string) ([]byte, bool) {
		value, ok := keys[provider]
		return []byte(value), ok
	}

	if got, ok := svc.providerCredential(ctx, modelAttempt{Provider: providerCerebras}, "caller-openrouter"); !ok || got != "vault-cerebras" {
		t.Fatalf("Cerebras credential = %q, %v", got, ok)
	}
	keys["cerebras"] = "rotated-cerebras"
	if got, ok := svc.providerCredential(ctx, modelAttempt{Provider: providerCerebras}, "caller-openrouter"); !ok || got != "rotated-cerebras" {
		t.Fatalf("rotated credential was not resolved at attempt time: %q, %v", got, ok)
	}
	if got, _ := svc.providerCredential(ctx, modelAttempt{Provider: providerGroq}, "caller-openrouter"); got != "vault-groq" {
		t.Fatalf("Groq received wrong provider credential: %q", got)
	}
	if got, _ := svc.providerCredential(ctx, modelAttempt{Provider: providerOpenRouter}, "caller-openrouter"); got != "caller-openrouter" {
		t.Fatalf("OpenRouter request override changed: %q", got)
	}

	t.Setenv("CEREBRAS_API_KEY", "environment-cerebras")
	if got, _ := svc.providerCredential(ctx, modelAttempt{Provider: providerCerebras}, "caller-openrouter"); got != "environment-cerebras" {
		t.Fatalf("environment credential must override vault: %q", got)
	}
	publicCtx := WithAmbientProviderCredentials(context.Background(), false)
	if got, ok := svc.providerCredential(publicCtx, modelAttempt{Provider: providerGroq}, "caller-openrouter"); ok || got != "" {
		t.Fatalf("vault credential escaped onto disallowed public request: %q, %v", got, ok)
	}
}

type multiProviderQuotaTransport struct {
	calls []providerCall
}

type providerCall struct {
	host          string
	authorization string
	model         string
}

type successfulProviderTransport struct {
	calls []providerCall
}

type providerSchemaErrorTransport struct{}

func (providerSchemaErrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":"property 'messages.3.assistant.reasoning_content' is unsupported"}`)),
	}, nil
}

func TestProviderSchemaErrorSkipsOnlyCurrentChainAndLeavesBreakerClosed(t *testing.T) {
	t.Setenv("PROXY_CEREBRAS_URL", "https://api.cerebras.ai/v1/chat/completions")
	t.Setenv("CEREBRAS_API_KEY", "cerebras-key")
	svc := NewRouterService()
	t.Cleanup(svc.Close)
	svc.Client = &http.Client{Transport: providerSchemaErrorTransport{}}

	err := svc.executeAttempt(
		WithAmbientProviderCredentials(context.Background(), true),
		httptest.NewRecorder(),
		[]byte(`{"messages":[]}`),
		"cerebras-key",
		modelAttempt{Profile: "coding", Provider: providerCerebras, Model: "gpt-oss-120b"},
	)
	var attErr *attemptError
	if !errors.As(err, &attErr) {
		t.Fatalf("error = %v, want attemptError", err)
	}
	if !attErr.SkipProvider || attErr.ProviderUnavailable {
		t.Fatalf("schema error flags = %+v, want request-only provider skip", attErr)
	}
	if state := svc.providerBreakerSnapshot(providerCerebras); state.State != "closed" || state.ConsecutiveFailures != 0 {
		t.Fatalf("schema error must not poison provider breaker: %+v", state)
	}
}

func (t *successfulProviderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(req.Body).Decode(&payload)
	t.calls = append(t.calls, providerCall{host: req.URL.Host, authorization: req.Header.Get("Authorization"), model: payload.Model})
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)),
	}, nil
}

func (t *multiProviderQuotaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(req.Body).Decode(&payload)
	t.calls = append(t.calls, providerCall{
		host:          req.URL.Host,
		authorization: req.Header.Get("Authorization"),
		model:         payload.Model,
	})

	if req.URL.Host == "openrouter.ai" {
		body := strings.Replace(realDailyFreeQuota429, "1786233600000", strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10), 1)
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"rescued"}}]}`)),
	}, nil
}

func TestDailyQuotaSkipsRemainingProviderModelsAndFallsBackToCerebras(t *testing.T) {
	t.Setenv("CEREBRAS_API_KEY", "cerebras-secret")
	t.Setenv("GROQ_API_KEY", "groq-secret")
	transport := &multiProviderQuotaTransport{}
	svc := newTestService(t, &http.Client{Transport: transport}, policyConfig{
		DefaultProfile: "coding",
		Profiles:       map[string][]string{"coding": {"legacy"}},
		Aliases:        map[string]string{"default": "coding", "coding": "coding"},
	})
	svc.setProviderProfiles(providerProfiles{
		"coding": {
			{Provider: providerOpenRouter, Model: "first:free"},
			{Provider: providerOpenRouter, Model: "second:free"},
			{Provider: providerCerebras, Model: "gpt-oss-120b"},
			{Provider: providerGroq, Model: "openai/gpt-oss-120b"},
		},
	})

	req := trustedRequest(http.MethodPost, "/v1/chat/completions", `{"model":"coding","messages":[{"role":"user","content":"hi"}]}`)
	req = req.WithContext(WithAmbientProviderCredentials(req.Context(), true))
	rec := newHeaderSnapshotRecorder()
	svc.RouteRequest(rec, req, "openrouter-secret")

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "rescued") {
		t.Fatalf("expected Cerebras rescue response, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(transport.calls) != 2 {
		t.Fatalf("daily provider quota should skip sibling OpenRouter models, calls=%+v", transport.calls)
	}
	if first := transport.calls[0]; first.host != "openrouter.ai" || first.authorization != "Bearer openrouter-secret" || first.model != "first:free" {
		t.Errorf("unexpected OpenRouter attempt: %+v", first)
	}
	if second := transport.calls[1]; second.host != "api.cerebras.ai" || second.authorization != "Bearer cerebras-secret" || second.model != "gpt-oss-120b" {
		t.Errorf("unexpected Cerebras attempt: %+v", second)
	}
	if state := svc.providerBreakerSnapshot(providerOpenRouter); state.State != "closed" {
		t.Errorf("quota exhaustion must not open the reliability/provider breaker: %+v", state)
	}
	quota := svc.quotaHealth(providerOpenRouter, time.Now())
	if len(quota) == 0 || quota[0].ModelOrPool != quotaFreePool || quota[0].Remaining != 0 {
		t.Errorf("OpenRouter free-pool exhaustion was not preserved in the quota ledger: %+v", quota)
	}
}

func TestRouteUsesNormalizedQuotaPressureInsteadOfRawOneToOneBalance(t *testing.T) {
	t.Setenv("CEREBRAS_API_KEY", "cerebras-secret")
	t.Setenv("GROQ_API_KEY", "groq-secret")
	t.Setenv("PROXY_CEREBRAS_URL", "https://api.cerebras.ai/v1/chat/completions")
	t.Setenv("PROXY_GROQ_URL", "https://api.groq.com/openai/v1/chat/completions")
	transport := &successfulProviderTransport{}
	svc := newTestService(t, &http.Client{Transport: transport}, policyConfig{
		DefaultProfile: "coding",
		Profiles:       map[string][]string{"coding": {"legacy"}},
		Aliases:        map[string]string{"default": "coding", "coding": "coding"},
	})
	svc.setProviderProfiles(providerProfiles{"coding": {
		{Provider: providerOpenRouter, Model: "or:free"},
		{Provider: providerCerebras, Model: "gpt-oss-120b"},
		{Provider: providerGroq, Model: "openai/gpt-oss-120b"},
	}})

	const requests = 20
	for i := 0; i < requests; i++ {
		req := trustedRequest(http.MethodPost, "/v1/chat/completions", `{"model":"coding","messages":[{"role":"user","content":"hi"}]}`)
		req = req.WithContext(WithAmbientProviderCredentials(req.Context(), true))
		rec := newHeaderSnapshotRecorder()
		svc.RouteRequest(rec, req, "openrouter-secret")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d failed: %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if len(transport.calls) != requests {
		t.Fatalf("expected one successful upstream call per request, got %+v", transport.calls)
	}
	counts := map[string]int{}
	for _, call := range transport.calls {
		counts[call.host]++
	}
	for _, host := range []string{"openrouter.ai", "api.cerebras.ai", "api.groq.com"} {
		if counts[host] == 0 {
			t.Fatalf("quota-aware exploration never selected %s; counts=%v", host, counts)
		}
	}
	if counts["openrouter.ai"] == counts["api.cerebras.ai"] && counts["api.cerebras.ai"] == counts["api.groq.com"] {
		t.Fatalf("scheduler regressed to raw 1:1:1 instead of normalized pressure: %v", counts)
	}
}

func TestProviderBreakerAndScoresAreIsolatedByProvider(t *testing.T) {
	svc := NewRouterService()
	t.Cleanup(svc.Close)
	a := modelAttempt{Profile: "coding", Provider: providerCerebras, Model: "shared-model"}
	b := modelAttempt{Profile: "coding", Provider: providerGroq, Model: "shared-model"}
	if svc.breakerKey(a) == svc.breakerKey(b) {
		t.Fatalf("provider must be part of breaker identity: %q", svc.breakerKey(a))
	}
}

func TestProviderAttemptConsumptionCountsActualFallbackCalls(t *testing.T) {
	svc := NewRouterService()
	t.Cleanup(svc.Close)
	svc.Client = &http.Client{Transport: &multiProviderQuotaTransport{}}
	svc.TargetResolver = DefaultAttemptTargetResolver{}
	t.Setenv("PROXY_CEREBRAS_URL", "https://api.cerebras.ai/v1/chat/completions")
	t.Setenv("CEREBRAS_API_KEY", "cerebras-secret")
	ctx := WithAmbientProviderCredentials(context.Background(), true)

	first := modelAttempt{Profile: "coding", Provider: providerOpenRouter, Model: "first:free", AttemptIndex: 1, BalanceReserved: true}
	svc.reserveProviderBalance(providerOpenRouter)
	_ = svc.executeAttempt(ctx, httptest.NewRecorder(), []byte(`{"messages":[]}`), "openrouter-secret", first)
	second := modelAttempt{Profile: "coding", Provider: providerCerebras, Model: "gpt-oss-120b", AttemptIndex: 2}
	_ = svc.executeAttempt(ctx, httptest.NewRecorder(), []byte(`{"messages":[]}`), "openrouter-secret", second)

	if svc.providerAttemptCount(providerOpenRouter) != 1 || svc.providerAttemptCount(providerCerebras) != 1 {
		t.Fatalf("reserved primary and actual fallback should each count once: %+v", svc.providerAttempts)
	}
	if svc.providerBalanceCount(providerOpenRouter) != 1 || svc.providerBalanceCount(providerCerebras) != 1 {
		t.Fatalf("primary reservation and fallback should balance once each: %+v", svc.providerBalance)
	}
}
