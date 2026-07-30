package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cervorules "github.com/cervantesh/cervo-rules/v3/core"
	cervoruntime "github.com/cervantesh/cervo-rules/v3/runtime"
)

type recordingTransport struct {
	calls int
	body  string
	urls  []string
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls++
	t.urls = append(t.urls, req.URL.String())
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		t.body = string(body)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`)),
	}, nil
}

type sequenceTransport struct {
	statuses []int
	urls     []string
}

type staticSideEffects struct {
	metadata map[string]any
}

type fixedPolicyEngine struct {
	decision policyDecision
}

func (e fixedPolicyEngine) Decide(_ context.Context, req cervorules.Request) (cervorules.DecisionResult, error) {
	return cervorules.NewDecisionResult(req, e.decision.coreDecision()), nil
}

func (e fixedPolicyEngine) DecideWithOptions(_ context.Context, req cervorules.Request, options cervorules.DecisionOptions) (cervorules.DecisionResult, error) {
	return cervorules.NewDecisionResult(req, e.decision.coreDecision(), cervorules.WithTrace(options.TraceEnabled()), cervorules.WithObservation(options.ObservationEnabled())), nil
}

func allowedTestDecision(provider cervorules.Executor, limits Limits) policyDecision {
	decision := defaultAllowedDecision()
	decision.Executor = provider
	decision.Reason = "test policy allowed request"
	decision.RuleID = "test.allow"
	decision.Limits = limits
	return decision
}

func (s staticSideEffects) Extract(_ context.Context, _ string) map[string]any {
	return s.metadata
}

func testOperationTargets() map[cervorules.Operation]cervorules.Target {
	return map[cervorules.Operation]cervorules.Target{
		capChatCompletion: serviceCervoCore,
		capSecretLookup:   serviceCervoVaultAPI,
	}
}

func mustBuildPolicyFlow(t *testing.T, cfg ruleRuntimeConfig) cervorules.Engine {
	t.Helper()
	engine, err := buildPolicyFlow(cfg)
	if err != nil {
		t.Fatalf("buildPolicyFlow failed: %v", err)
	}
	return engine
}

func (t *sequenceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.urls = append(t.urls, req.URL.String())
	status := http.StatusOK
	if len(t.statuses) > 0 {
		status = t.statuses[0]
		t.statuses = t.statuses[1:]
	}
	body := `{"choices":[]}`
	if status != http.StatusOK {
		body = `upstream unavailable`
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func TestRouteRequestWithProvider_DeniesPolicyBeforeUpstream(t *testing.T) {
	transport := &recordingTransport{}
	svc := NewRouterService()
	svc.Client = &http.Client{Transport: transport}
	cfg := ruleRuntimeConfig{PolicyRuntimeConfig: cervoruntime.PolicyRuntimeConfig{
		OperationTargets: testOperationTargets(),
		TrustedUsers:     []string{"cervantes"},
		DefaultExecutor:  providerOpenRouter,
	}}
	cfg = normalizeRuleConfig(cfg)
	svc.runtimeConfig = cfg
	svc.PolicyEngine = mustBuildPolicyFlow(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("X-Cervo-Capability", string(capSecretLookup))
	req.Header.Set("X-Cervo-User", "guest")
	rec := httptest.NewRecorder()

	svc.RouteRequestWithProvider(rec, req, "test-key", "")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected forbidden, got %d body=%s", rec.Code, rec.Body.String())
	}
	if transport.calls != 0 {
		t.Fatalf("expected denied request not to touch upstream, got %d calls", transport.calls)
	}
}

func TestRouteRequestWithProvider_AllowedPolicyTouchesUpstream(t *testing.T) {
	transport := &recordingTransport{}
	svc := NewRouterService()
	svc.Client = &http.Client{Transport: transport}
	cfg := normalizeRuleConfig(ruleRuntimeConfig{PolicyRuntimeConfig: cervoruntime.PolicyRuntimeConfig{OperationTargets: testOperationTargets(), TrustedUsers: []string{"cervantes"}, DefaultExecutor: providerOpenRouter}})
	svc.runtimeConfig = cfg
	svc.PolicyEngine = mustBuildPolicyFlow(t, cfg)
	svc.setModelPolicyConfig(policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a"}},
		Aliases:        map[string]string{"simple": "simple", "default": "simple"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("X-Cervo-Capability", string(capChatCompletion))
	req.Header.Set("X-Cervo-User", "cervantes")
	rec := httptest.NewRecorder()

	svc.RouteRequestWithProvider(rec, req, "test-key", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected ok, got %d body=%s", rec.Code, rec.Body.String())
	}
	if transport.calls != 1 {
		t.Fatalf("expected allowed request to touch upstream once, got %d calls", transport.calls)
	}
}

func TestRouteRequestWithProvider_ModelPolicySelectsUpstreamModel(t *testing.T) {
	transport := &recordingTransport{}
	svc := NewRouterService()
	svc.Client = &http.Client{Transport: transport}
	cfg := normalizeRuleConfig(ruleRuntimeConfig{PolicyRuntimeConfig: cervoruntime.PolicyRuntimeConfig{
		OperationTargets: testOperationTargets(),
		TrustedUsers:     []string{"cervantes"},
		DefaultExecutor:  providerOpenRouter,
	}})
	svc.runtimeConfig = cfg
	svc.PolicyEngine = mustBuildPolicyFlow(t, cfg)
	svc.setModelPolicyConfig(policyConfig{
		DefaultProfile: "simple",
		Profiles: map[string][]string{
			"simple": {"model-simple"},
			"coding": {"model-coding"},
		},
		Aliases: map[string]string{"default": "simple", "simple": "simple", "coding": "coding"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("X-Cervo-Capability", string(capChatCompletion))
	req.Header.Set("X-Cervo-User", "cervantes")
	rec := httptest.NewRecorder()

	svc.RouteRequestWithProvider(rec, req, "test-key", "coding")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected ok, got %d body=%s", rec.Code, rec.Body.String())
	}
	var upstream map[string]any
	if err := json.Unmarshal([]byte(transport.body), &upstream); err != nil {
		t.Fatalf("invalid upstream body %q: %v", transport.body, err)
	}
	if upstream["model"] != "model-coding" {
		t.Fatalf("expected model-coding upstream, got body=%s", transport.body)
	}
}

func TestRouteRequestWithProvider_InjectsConfiguredSideEffects(t *testing.T) {
	transport := &recordingTransport{}
	svc := NewRouterService()
	svc.Client = &http.Client{Transport: transport}
	svc.SideEffects = staticSideEffects{metadata: map[string]any{"source": "test"}}
	cfg := normalizeRuleConfig(ruleRuntimeConfig{PolicyRuntimeConfig: cervoruntime.PolicyRuntimeConfig{
		OperationTargets: testOperationTargets(),
		TrustedUsers:     []string{"cervantes"},
		DefaultExecutor:  providerOpenRouter,
	}})
	svc.runtimeConfig = cfg
	svc.PolicyEngine = mustBuildPolicyFlow(t, cfg)
	svc.setModelPolicyConfig(policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("X-Cervo-Capability", string(capChatCompletion))
	req.Header.Set("X-Cervo-User", "cervantes")
	rec := httptest.NewRecorder()

	svc.RouteRequestWithProvider(rec, req, "test-key", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected ok, got %d body=%s", rec.Code, rec.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("invalid response body: %v", err)
	}
	choice := response["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	metadata := message["metadata"].(map[string]any)
	if metadata["source"] != "test" {
		t.Fatalf("expected injected side-effect metadata, got %+v", metadata)
	}
}

func TestRouteRequestWithProvider_RejectsMaxTokensOverPolicy(t *testing.T) {
	transport := &recordingTransport{}
	svc := NewRouterService()
	svc.Client = &http.Client{Transport: transport}
	svc.runtimeConfig = normalizeRuleConfig(ruleRuntimeConfig{Limits: Limits{MaxTokens: 10}})
	svc.PolicyEngine = fixedPolicyEngine{decision: allowedTestDecision(providerOpenRouter, Limits{MaxTokens: 10})}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[],"max_tokens":11}`))
	req.Header.Set("X-Cervo-Capability", string(capChatCompletion))
	req.Header.Set("X-Cervo-User", "cervantes")
	rec := httptest.NewRecorder()

	svc.RouteRequestWithProvider(rec, req, "test-key", "")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected forbidden, got %d body=%s", rec.Code, rec.Body.String())
	}
	if transport.calls != 0 {
		t.Fatalf("expected limited request not to touch upstream, got %d calls", transport.calls)
	}
}

func TestRouteRequestWithProvider_RejectsBodyOverPolicy(t *testing.T) {
	transport := &recordingTransport{}
	svc := NewRouterService()
	svc.Client = &http.Client{Transport: transport}
	svc.runtimeConfig = normalizeRuleConfig(ruleRuntimeConfig{Limits: Limits{MaxBodyBytes: 10}})
	svc.PolicyEngine = fixedPolicyEngine{decision: allowedTestDecision(providerOpenRouter, Limits{MaxBodyBytes: 10})}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("X-Cervo-Capability", string(capChatCompletion))
	req.Header.Set("X-Cervo-User", "cervantes")
	rec := httptest.NewRecorder()

	svc.RouteRequestWithProvider(rec, req, "test-key", "")

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected payload too large, got %d body=%s", rec.Code, rec.Body.String())
	}
	if transport.calls != 0 {
		t.Fatalf("expected limited request not to touch upstream, got %d calls", transport.calls)
	}
}

func TestRouteRequestWithProvider_RejectsDisallowedStreamToolsAndImages(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "stream", body: `{"messages":[],"stream":true}`},
		{name: "tools", body: `{"messages":[],"tools":[{"type":"function"}]}`},
		{name: "images", body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &recordingTransport{}
			svc := NewRouterService()
			svc.Client = &http.Client{Transport: transport}
			disallowOptional := Limits{MaxBodyBytes: 1 << 30}
			svc.runtimeConfig = normalizeRuleConfig(ruleRuntimeConfig{Limits: disallowOptional})
			svc.PolicyEngine = fixedPolicyEngine{decision: allowedTestDecision(providerOpenRouter, disallowOptional)}

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tt.body))
			req.Header.Set("X-Cervo-Capability", string(capChatCompletion))
			req.Header.Set("X-Cervo-User", "cervantes")
			rec := httptest.NewRecorder()

			svc.RouteRequestWithProvider(rec, req, "test-key", "")

			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected forbidden, got %d body=%s", rec.Code, rec.Body.String())
			}
			if transport.calls != 0 {
				t.Fatalf("expected limited request not to touch upstream, got %d calls", transport.calls)
			}
		})
	}
}

func TestBuildModelAttemptsFromDecision_ExpandsExecutorFallbacks(t *testing.T) {
	svc := NewRouterService()
	decision := policyDecision{
		Executor:          providerOpenAI,
		FallbackExecutors: []cervorules.Executor{providerOpenRouter},
	}
	svc.setModelPolicyConfig(policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a", "model-b"}},
		Aliases:        map[string]string{"simple": "simple", "default": "simple"},
	})

	attempts := svc.planModelAttempts(decision, "simple", "auto")
	if len(attempts) != 4 {
		t.Fatalf("expected 4 provider/model attempts, got %+v", attempts)
	}
	if attempts[0].Provider != providerOpenAI || attempts[0].Model != "model-a" {
		t.Fatalf("unexpected first attempt: %+v", attempts[0])
	}
	if attempts[1].Provider != providerOpenRouter || attempts[1].Model != "model-a" {
		t.Fatalf("unexpected provider fallback attempt: %+v", attempts[1])
	}
}

func TestRouteRequestWithProvider_RetriesProviderFallback(t *testing.T) {
	transport := &sequenceTransport{statuses: []int{http.StatusBadGateway, http.StatusOK}}
	svc := NewRouterService()
	svc.Client = &http.Client{Transport: transport}
	decision := allowedTestDecision(providerOpenAI, Limits{})
	decision.FallbackExecutors = []cervorules.Executor{providerOpenRouter}
	decision.RetryPolicy = RetryPolicy{MaxAttempts: 2, RetryHTTPStatuses: []int{http.StatusBadGateway}}
	svc.runtimeConfig = normalizeRuleConfig(ruleRuntimeConfig{RetryPolicy: decision.RetryPolicy})
	svc.PolicyEngine = fixedPolicyEngine{decision: decision}
	svc.setModelPolicyConfig(policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("X-Cervo-Capability", string(capChatCompletion))
	req.Header.Set("X-Cervo-User", "cervantes")
	rec := httptest.NewRecorder()

	svc.RouteRequestWithProvider(rec, req, "test-key", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected fallback success, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(transport.urls) != 2 {
		t.Fatalf("expected two upstream attempts, got %+v", transport.urls)
	}
	if !strings.Contains(transport.urls[0], "api.openai.com") || !strings.Contains(transport.urls[1], "openrouter.ai") {
		t.Fatalf("expected openai then openrouter URLs, got %+v", transport.urls)
	}
}
