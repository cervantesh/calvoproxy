package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	cervorules "github.com/cervantesh/cervo-rules/v3/core"
	cervoruntime "github.com/cervantesh/cervo-rules/v3/runtime"
)

func TestLoadPolicyConfig_EnvOverridesAndAliasCleanup(t *testing.T) {
	profiles, _ := json.Marshal(map[string][]string{
		"coding": {"m1", "m2"},
		"vision": {"m3"},
	})
	aliases, _ := json.Marshal(map[string]string{
		"code":    "coding",
		"broken":  "missing",
		"see":     "vision",
		"default": "vision",
	})
	t.Setenv("PROXY_PROVIDER_PROFILES_JSON", string(profiles))
	t.Setenv("PROXY_PROVIDER_ALIASES_JSON", string(aliases))
	t.Setenv("PROXY_DEFAULT_PROFILE", "vision")

	cfg := loadPolicyConfig()
	if cfg.DefaultProfile != "vision" {
		t.Fatalf("unexpected default profile: %s", cfg.DefaultProfile)
	}
	if got := cfg.Aliases["code"]; got != "coding" {
		t.Fatalf("expected code alias to remain, got %q", got)
	}
	if _, ok := cfg.Aliases["broken"]; ok {
		t.Fatalf("expected broken alias to be removed")
	}
	if got := cfg.Aliases["default"]; got != "vision" {
		t.Fatalf("expected default alias to point to vision, got %q", got)
	}
}

func TestLoadPolicyConfig_CervoModelPolicyEnvOverrides(t *testing.T) {
	t.Setenv("CERVO_MODEL_DEFAULT_PROFILE", "coding")
	t.Setenv("CERVO_MODEL_PROFILES_JSON", `{"simple":["model-simple"],"coding":["model-coder","model-fallback"]}`)
	t.Setenv("CERVO_MODEL_ALIASES_JSON", `{"code":"coding","fast":"simple"}`)

	cfg := loadPolicyConfig()
	if cfg.DefaultProfile != "coding" {
		t.Fatalf("unexpected default profile: %s", cfg.DefaultProfile)
	}
	if got := cfg.Profiles["coding"]; len(got) != 2 || got[0] != "model-coder" || got[1] != "model-fallback" {
		t.Fatalf("unexpected coding chain: %+v", got)
	}
	if got := cfg.Aliases["code"]; got != "coding" {
		t.Fatalf("expected code alias to point to coding, got %q", got)
	}
	if got := cfg.Aliases["default"]; got != "coding" {
		t.Fatalf("expected normalized default alias to point to coding, got %q", got)
	}
}

func TestLoadPolicyConfig_InvalidDefaultFallsBack(t *testing.T) {
	t.Setenv("PROXY_DEFAULT_PROFILE", "missing")
	cfg := loadPolicyConfig()
	if cfg.DefaultProfile == "missing" {
		t.Fatalf("expected fallback default profile, got %q", cfg.DefaultProfile)
	}
	if _, ok := cfg.Profiles[cfg.DefaultProfile]; !ok {
		t.Fatalf("default profile %q missing from profiles", cfg.DefaultProfile)
	}
}

func TestLoadModelPolicyRuntime_StrictReportsWarnings(t *testing.T) {
	t.Setenv("CERVO_MODEL_POLICY_STRICT", "true")
	t.Setenv("CERVO_MODEL_DEFAULT_PROFILE", "missing")
	t.Setenv("CERVO_MODEL_PROFILES_JSON", `{"simple":["model-simple"]}`)

	runtime := loadModelPolicyRuntime()
	if !runtime.Strict {
		t.Fatal("expected strict mode")
	}
	if len(runtime.Warnings) == 0 {
		t.Fatal("expected validation warnings")
	}

	svc := NewRouterService()
	health := svc.Health()
	if health.Ready {
		t.Fatalf("expected strict invalid model policy to mark service not ready: %+v", health)
	}
	if !health.ModelPolicy.Strict || len(health.ModelPolicy.ValidationWarnings) == 0 {
		t.Fatalf("expected model policy health to expose strict warnings: %+v", health.ModelPolicy)
	}
}

func TestLoadModelPolicyRuntime_StrictTreatsInvalidJSONAsWarning(t *testing.T) {
	t.Setenv("CERVO_MODEL_POLICY_STRICT", "true")
	t.Setenv("CERVO_MODEL_PROFILES_JSON", `{"simple":`)

	runtime := loadModelPolicyRuntime()
	if !runtime.Strict || len(runtime.Warnings) == 0 {
		t.Fatalf("expected strict runtime warning for invalid JSON: %+v", runtime)
	}
	if runtime.Warnings[0].Code != "runtime_config_invalid" {
		t.Fatalf("expected runtime_config_invalid warning, got %+v", runtime.Warnings)
	}
}

func TestDefaultModelConfigComesFromEmbeddedJSON(t *testing.T) {
	cfg := defaultModelConfig()
	if cfg.DefaultProfile != "simple" {
		t.Fatalf("unexpected default profile: %q", cfg.DefaultProfile)
	}
	if len(cfg.Profiles["coding"]) == 0 {
		t.Fatalf("expected coding profile from embedded defaults: %+v", cfg.Profiles)
	}
}

func TestUtilityHelpers(t *testing.T) {
	if got := extractMessageText([]interface{}{
		map[string]interface{}{"type": "text", "text": "hello"},
		map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "x"}},
		map[string]interface{}{"type": "text", "text": "world"},
	}); got != "hello world" {
		t.Fatalf("unexpected extracted text: %q", got)
	}
	if !contentContainsImage([]interface{}{map[string]interface{}{"type": "image", "image": "x"}}) {
		t.Fatal("expected image content to be detected")
	}
	if got := envInt("ROUTER_MISSING_INT", 7); got != 7 {
		t.Fatalf("expected fallback env int, got %d", got)
	}
	t.Setenv("ROUTER_INT", "12")
	if got := envInt("ROUTER_INT", 7); got != 12 {
		t.Fatalf("expected parsed env int, got %d", got)
	}
	t.Setenv("ROUTER_DURATION", "9")
	if got := envDurationSeconds("ROUTER_DURATION", time.Second); got != 9*time.Second {
		t.Fatalf("unexpected duration: %s", got)
	}
	t.Setenv("ROUTER_BOOL", "yes")
	if !envBool("ROUTER_BOOL", false) {
		t.Fatal("expected envBool to parse yes")
	}
}

func TestNormalizeRuleConfig_AppliesBaseDefaults(t *testing.T) {
	cfg := normalizeRuleConfig(ruleRuntimeConfig{})

	if cfg.DefaultTimeout != defaultPolicyTimeout {
		t.Fatalf("expected default timeout %s, got %s", defaultPolicyTimeout, cfg.DefaultTimeout)
	}
	if cfg.DefaultExecutor != providerOpenRouter {
		t.Fatalf("expected default executor %s, got %s", providerOpenRouter, cfg.DefaultExecutor)
	}
	if cfg.BreakerPolicy.FailureThreshold != 3 {
		t.Fatalf("expected default breaker policy, got %+v", cfg.BreakerPolicy)
	}
}

func TestNormalizeRuleConfig_PreservesExplicitRoutes(t *testing.T) {
	cfg := normalizeRuleConfig(ruleRuntimeConfig{
		PolicyRuntimeConfig: runtimeConfig(cervorules.Executor(providerOpenAI), map[cervorules.Operation]cervorules.Target{
			capPlanning:     serviceCrashitoBrain,
			capMediaRequest: serviceCervoMedia,
		}),
		DefaultTimeout: 7 * time.Second,
	})

	if cfg.DefaultTimeout != 7*time.Second {
		t.Fatalf("expected explicit timeout, got %s", cfg.DefaultTimeout)
	}
	if cfg.DefaultExecutor != providerOpenAI {
		t.Fatalf("expected explicit executor, got %s", cfg.DefaultExecutor)
	}
	if got := cfg.OperationTargets[capPlanning]; got != serviceCrashitoBrain {
		t.Fatalf("expected planning override, got %s", got)
	}
	if got := cfg.OperationTargets[capMediaRequest]; got != serviceCervoMedia {
		t.Fatalf("expected media route to be preserved, got %s", got)
	}
}

func TestBuildPolicyFlow_RejectsUnknownVocabulary(t *testing.T) {
	_, err := buildPolicyFlow(ruleRuntimeConfig{
		PolicyRuntimeConfig: runtimeConfig(providerOpenRouter, map[cervorules.Operation]cervorules.Target{
			capPlanning: cervorules.Target("unknown_backend"),
		}),
	})
	if err == nil {
		t.Fatal("expected unknown backend to fail vocabulary validation")
	}
}

func TestBuildPolicyFlow_AppliesRuntimeOverrides(t *testing.T) {
	cfg := ruleRuntimeConfig{
		PolicyRuntimeConfig: runtimeConfig(providerOpenAI, map[cervorules.Operation]cervorules.Target{
			capPlanning:     serviceCrashitoBrain,
			capMediaRequest: serviceCervoMedia,
		}),
		DefaultTimeout: 7 * time.Second,
		RetryPolicy:    RetryPolicy{MaxAttempts: 2},
		BreakerPolicy:  BreakerPolicy{FailureThreshold: 5, Eligible: true},
		Limits:         Limits{MaxTokens: 123, AllowImages: true},
	}
	cfg.ExecutorFallbacks = map[cervorules.Executor][]cervorules.Executor{providerOpenAI: {providerOpenRouter}}

	engine := mustBuildPolicyFlow(t, cfg)
	svc := &RouterService{runtimeConfig: normalizeRuleConfig(cfg)}
	planningResult, err := engine.Decide(testContext(), cervorules.Request{Operation: capPlanning})
	if err != nil {
		t.Fatalf("planning decision failed: %v", err)
	}
	planning := svc.proxyDecision(cervorules.Request{Operation: capPlanning}, planningResult.Decision)
	if !planning.Allow || planning.Target != serviceCrashitoBrain || planning.Executor != providerOpenAI {
		t.Fatalf("unexpected planning decision: %+v", planning)
	}
	if planning.Timeout != 7*time.Second || planning.RetryPolicy.MaxAttempts != 2 || planning.BreakerPolicy.FailureThreshold != 5 {
		t.Fatalf("runtime defaults not applied: %+v", planning)
	}
	if planning.Limits.MaxTokens != 123 || !planning.Limits.AllowImages {
		t.Fatalf("runtime limits not applied: %+v", planning.Limits)
	}
	if len(planning.FallbackExecutors) != 1 || planning.FallbackExecutors[0] != providerOpenRouter {
		t.Fatalf("runtime fallbacks not applied: %+v", planning.FallbackExecutors)
	}

	mediaResult, err := engine.Decide(testContext(), cervorules.Request{Operation: capMediaRequest})
	if err != nil {
		t.Fatalf("media decision failed: %v", err)
	}
	media := svc.proxyDecision(cervorules.Request{Operation: capMediaRequest}, mediaResult.Decision)
	if !media.Allow || media.Target != serviceCervoMedia {
		t.Fatalf("expected media override to enable route, got %+v", media)
	}
}

func TestLoadRuleConfig_EnvRuntimeOverridesFeedGeneratedPolicy(t *testing.T) {
	t.Setenv("PROXY_DEFAULT_PROVIDER", string(providerOpenAI))
	t.Setenv("PROXY_PLANNING_SERVICE", string(serviceCrashitoBrain))
	t.Setenv("PROXY_MEDIA_SERVICE", string(serviceCervoMedia))
	t.Setenv("PROXY_PROVIDER_FALLBACKS_JSON", `{"openai":["openrouter","anthropic"]}`)
	t.Setenv("PROXY_LIMITS_JSON", `{"max_tokens":321,"allow_images":true}`)

	cfg := normalizeRuleConfig(loadRuleConfig(9*time.Second, 4, 30*time.Second))
	engine := mustBuildPolicyFlow(t, cfg)
	svc := &RouterService{runtimeConfig: cfg}

	planningResult, err := engine.Decide(testContext(), cervorules.Request{Operation: capPlanning})
	if err != nil {
		t.Fatalf("planning decision failed: %v", err)
	}
	planning := svc.proxyDecision(cervorules.Request{Operation: capPlanning}, planningResult.Decision)
	if !planning.Allow || planning.Target != serviceCrashitoBrain || planning.Executor != providerOpenAI {
		t.Fatalf("unexpected planning env decision: %+v", planning)
	}
	if planning.Timeout != 9*time.Second || planning.BreakerPolicy.FailureThreshold != 4 {
		t.Fatalf("runtime env defaults not applied: %+v", planning)
	}
	if planning.Limits.MaxTokens != 321 || !planning.Limits.AllowImages {
		t.Fatalf("limits JSON not applied: %+v", planning.Limits)
	}
	if len(planning.FallbackExecutors) != 2 || planning.FallbackExecutors[0] != providerOpenRouter || planning.FallbackExecutors[1] != providerAnthropic {
		t.Fatalf("provider fallbacks JSON not applied: %+v", planning.FallbackExecutors)
	}
}

func TestBuildPolicyFlow_MediaRouteDisabledWithoutRuntimeService(t *testing.T) {
	engine := mustBuildPolicyFlow(t, ruleRuntimeConfig{})

	result, err := engine.Decide(testContext(), cervorules.Request{Operation: capMediaRequest})
	if err != nil {
		t.Fatalf("media decision failed: %v", err)
	}
	if result.Decision.Allow || result.Decision.Reason != "media backend not configured" {
		t.Fatalf("expected media route to remain disabled without runtime service, got %+v", result.Decision)
	}
}

func TestProxyHTTPClassifier_UsesProductionPathRules(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		target    string
		operation cervorules.Operation
	}{
		{name: "chat completions", method: http.MethodPost, target: "/v1/chat/completions", operation: capChatCompletion},
		{name: "messages", method: http.MethodPost, target: "/v1/messages", operation: capChatCompletion},
		{name: "embeddings", method: http.MethodPost, target: "/v1/embeddings", operation: capEmbedding},
		{name: "secret regex", method: http.MethodGet, target: "/internal/secret/read", operation: capSecretLookup},
		{name: "media regex", method: http.MethodPost, target: "/v1/images/generations", operation: capMediaRequest},
		{name: "planning regex", method: http.MethodGet, target: "/v1/models", operation: capPlanning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.target, nil)
			facts := proxyHTTPClassifier.FactsFromHTTPRequest(req)
			if facts.OperationHint != tt.operation {
				t.Fatalf("expected %s, got %s", tt.operation, facts.OperationHint)
			}
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	facts := proxyHTTPClassifier.FactsFromHTTPRequest(req)
	if facts.OperationHint != "" {
		t.Fatalf("GET chat completion should not match POST-only rule, got %s", facts.OperationHint)
	}
}

func runtimeConfig(defaultExecutor cervorules.Executor, routes map[cervorules.Operation]cervorules.Target) cervoruntime.PolicyRuntimeConfig {
	return cervoruntime.PolicyRuntimeConfig{
		DefaultExecutor:  defaultExecutor,
		OperationTargets: routes,
	}
}

func testContext() context.Context {
	return context.Background()
}
