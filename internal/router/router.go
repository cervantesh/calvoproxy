package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	cervomodelpolicy "github.com/cervantesh/cervo-model-policy"
)

func NewRouterService() *RouterService {
	config := breakerConfig{
		FailureThreshold: envInt("PROXY_BREAKER_FAILURE_THRESHOLD", 3),
		Cooldown:         envDurationSeconds("PROXY_BREAKER_COOLDOWN_SECONDS", 60*time.Second),
		RequestTimeout:   envDurationSeconds("PROXY_REQUEST_TIMEOUT_SECONDS", 45*time.Second),
	}
	if config.FailureThreshold < 1 {
		config.FailureThreshold = 1
	}
	config.TotalTimeout = totalTimeout(config.RequestTimeout)

	// Clone the default transport (never mutate the process-wide singleton) and
	// bound header arrival with ResponseHeaderTimeout instead of a whole-request
	// client Timeout. This lets streamed (SSE) responses run long — the old
	// http.Client.Timeout counted body reads and silently cut live streams at
	// RequestTimeout, defeating the server's disabled WriteTimeout.
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.ResponseHeaderTimeout = config.RequestTimeout
	base.TLSHandshakeTimeout = 10 * time.Second
	// The proxy fans out to a small set of upstream hosts (mostly one:
	// openrouter.ai), so raise the idle-connection pool well above the stdlib
	// default of 2 per host. Otherwise high concurrency churns thousands of
	// short-lived connections — exhausting ephemeral ports and inflating OS
	// threads — instead of reusing a warm pool. Tunable via
	// PROXY_MAX_IDLE_CONNS_PER_HOST.
	perHost := envInt("PROXY_MAX_IDLE_CONNS_PER_HOST", 128)
	if perHost < 2 {
		perHost = 2
	}
	base.MaxIdleConnsPerHost = perHost
	base.MaxIdleConns = perHost * 4
	base.IdleConnTimeout = 90 * time.Second

	transport := &GlobalBreakerTransport{
		Base:             base,
		FailureThreshold: config.FailureThreshold,
		Cooldown:         config.Cooldown,
	}

	policyEngine, runtimeConfig := loadPolicyFlow(config.RequestTimeout, config.FailureThreshold, config.Cooldown)
	modelRuntime := loadModelPolicyRuntime()
	if modelRuntime.Strict && len(modelRuntime.Warnings) > 0 {
		policyEngine = denyAllPolicyEngine("model policy strict validation failed")
	}
	providerPolicy := modelRuntime.Config
	modelPolicy := cervomodelpolicy.NewPolicy(providerPolicy)
	return &RouterService{
		// No blanket Timeout: streaming responses must not be capped by a
		// whole-request deadline. Header arrival is bounded by the transport's
		// ResponseHeaderTimeout; non-stream attempts get a per-attempt context
		// deadline in the fallback loop; streams get an idle timeout instead.
		Client:         &http.Client{Transport: transport},
		SideEffects:    sideEffectsFromEnv(),
		TargetResolver: DefaultAttemptTargetResolver{},
		PolicyEngine:   policyEngine,
		config:         config,
		policy:         providerPolicy,
		modelPolicy:    modelPolicy,
		modelWarnings:  modelRuntime.Warnings,
		modelStrict:    modelRuntime.Strict,
		runtimeConfig:  runtimeConfig,
		policyMetadata: generatedPolicyMetadata(),
		modelBreakers:  make(map[string]*modelBreakerState),
	}
}

// --- Route dispatch ---

func (s *RouterService) RouteRequest(w http.ResponseWriter, r *http.Request, apiKey string) {
	s.RouteRequestWithProvider(w, r, apiKey, "")
}

func (s *RouterService) RouteRequestWithProvider(w http.ResponseWriter, r *http.Request, apiKey string, provider string) {
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	tracer := otel.Tracer("calvoproxy/router")
	ctx, span := tracer.Start(ctx, "RouteRequest_Proxy")
	defer span.End()

	// Bound the request body before reading it into memory, so an oversized or
	// malicious payload can't OOM the process. Configurable via
	// PROXY_MAX_BODY_BYTES (default 10 MiB).
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes())
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Request body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}

	if strings.Contains(r.URL.Path, "embeddings") {
		decision, ok := s.authorizeOperationalRoute(ctx, w, r, requestFacts{
			OperationHint: capEmbedding,
		}, bodyBytes)
		if !ok {
			return
		}
		if decision.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, decision.Timeout)
			defer cancel()
		}
		s.tunnelToOpenRouterEmbeddings(ctx, w, bodyBytes, apiKey)
		return
	}

	if strings.Contains(r.URL.Path, "messages") {
		_, ok := s.authorizeOperationalRoute(ctx, w, r, requestFacts{
			OperationHint: capChatCompletion,
		}, bodyBytes)
		if !ok {
			return
		}
		// No total deadline: the Anthropic tunnel streams via streamCopy, which
		// is bounded by header + idle timeouts (a fixed deadline would cut long
		// live streams). These messages/embeddings tunnels are dumb pass-throughs
		// — they do NOT run the model chain, breaker-eligible scoring or
		// multi-model fallback; only the per-host transport breaker applies.
		slog.InfoContext(ctx, "[CalvoProxy] 🚀 Anthropic Tunnel Active")
		s.tunnelToOpenRouterMessages(ctx, w, bodyBytes, apiKey)
		return
	}

	var reqBody map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &reqBody); err != nil {
		slog.WarnContext(ctx, "[CalvoProxy] Invalid JSON received in request body")
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	messagesRaw, _ := reqBody["messages"].([]interface{})
	category := s.determineProfile(r, messagesRaw, provider)
	requestedModel, _ := reqBody["model"].(string)
	category, requestedModel = s.resolveModelAlias(category, requestedModel)
	stream, _ := reqBody["stream"].(bool)
	hasTools := hasRequestTools(reqBody)
	hasImages := hasImageContent(messagesRaw)
	policyFacts := requestPolicyFacts(reqBody, category, requestedModel, stream, hasTools, hasImages, int64(len(bodyBytes)))

	decision, ok := s.authorizeOperationalRoute(ctx, w, r, requestFacts{
		// Force the chat-completion operation so profile-in-path routes
		// (/v1/coding/chat/completions, /v1/agent/..., …) are classified the
		// same as /v1/chat/completions. Without this hint the policy derives
		// the operation from the path prefix and denies every profile route
		// with "no route for operation".
		OperationHint: capChatCompletion,
		Stream:        stream,
		Metadata:      policyFacts.Metadata,
	}, bodyBytes, policyFacts.RequestedLimits)
	if !ok {
		return
	}
	// Non-streaming requests get an overall wall-clock budget across the whole
	// fallback chain (each attempt is additionally capped per-attempt in the
	// loop). Streaming requests get NO total deadline here — they are bounded by
	// the transport's header timeout and a per-chunk idle timeout instead, so a
	// long-but-live stream is never cut mid-response.
	if !stream {
		if total := s.config.TotalTimeout; total > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, total)
			defer cancel()
		}
	}
	attemptsToTry := s.planModelAttempts(decision, category, requestedModel)
	availableModels := s.filterAvailableAttempts(attemptsToTry)
	// Reorder the breaker-eligible chain by reliability score (most reliable
	// first) before truncating to MaxAttempts, so flaky models sink to the back.
	availableModels = s.rankAttemptsByScore(availableModels)
	if decision.RetryPolicy.MaxAttempts > 0 && len(availableModels) > decision.RetryPolicy.MaxAttempts {
		availableModels = availableModels[:decision.RetryPolicy.MaxAttempts]
	}

	slog.InfoContext(ctx, "[CalvoProxy] 🏷️ Resolving Route",
		slog.String("category", category),
		slog.String("policy_target", string(decision.Target)),
		slog.String("executor", string(decision.Executor)),
		slog.String("rule_id", decision.RuleID),
		slog.String("audit_class", string(decision.AuditClass)),
		slog.String("policy_reason", decision.Reason),
		slog.Any("availableRoute", availableModels),
	)

	if len(availableModels) == 0 {
		writeJSONError(w, http.StatusServiceUnavailable, "All models are temporarily rate-limited or unhealthy. Cooling down before retry.")
		return
	}

	perAttempt := s.config.RequestTimeout
	if decision.Timeout > 0 && decision.Timeout < perAttempt {
		perAttempt = decision.Timeout
	}
	err = s.executeFallbacks(ctx, w, FallbackExecution{
		RequestBody:       reqBody,
		APIKey:            apiKey,
		Attempts:          availableModels,
		RetryPolicy:       decision.RetryPolicy,
		Stream:            stream,
		PerAttemptTimeout: perAttempt,
	})
	if err == nil {
		return
	}

	statusCode, message := fallbackErrorResponse(err)
	slog.ErrorContext(ctx, "[CalvoProxy] 🚨 CRITICAL: All fallback models failed", slog.String("profile", category))
	writeJSONError(w, statusCode, message)
}

func (s *RouterService) determineProfile(r *http.Request, messages []interface{}, forcedProfile string) string {
	selected := s.classifyPrompt(messages)
	if alias, ok := s.resolveProfileAlias(forcedProfile); ok {
		selected = alias
	}
	for _, param := range []string{r.URL.Query().Get("provider"), r.URL.Query().Get("category")} {
		if alias, ok := s.resolveProfileAlias(param); ok {
			selected = alias
		}
	}
	for _, header := range []string{r.Header.Get("X-Cervo-Provider"), r.Header.Get("X-Cervo-Category")} {
		if alias, ok := s.resolveProfileAlias(header); ok {
			selected = alias
		}
	}
	pol := s.getPolicy()
	if s.hasImageContent(messages) {
		if _, ok := pol.Profiles["vision"]; ok {
			selected = "vision"
		}
	}
	if _, ok := pol.Profiles[selected]; !ok {
		return pol.DefaultProfile
	}
	return selected
}

func (s *RouterService) resolveModelAlias(category string, requestedModel string) (string, string) {
	if reqModelStr := strings.TrimSpace(requestedModel); reqModelStr != "" {
		parts := strings.SplitN(reqModelStr, "/", 2)
		if len(parts) == 2 {
			prefix := strings.ToLower(strings.TrimSpace(parts[0]))
			if prefix == "calvoproxy" {
				if alias, ok := s.resolveProfileAlias(parts[1]); ok {
					return alias, "auto"
				}
			}
		}
	}
	return s.activeModelPolicy().ResolveModelAlias(category, requestedModel)
}

func (s *RouterService) planModelAttempts(decision policyDecision, category string, requestedModel string) []modelAttempt {
	planner := s.AttemptPlanner
	if planner == nil {
		planner = PolicyModelAttemptPlanner{Policy: s.getPolicy(), model: s.activeModelPolicy()}
	}
	return planner.Plan(decision, category, requestedModel)
}

func (s *RouterService) activeModelPolicy() *cervomodelpolicy.Policy {
	s.policyMu.RLock()
	mp := s.modelPolicy
	pol := s.policy
	s.policyMu.RUnlock()
	if mp != nil {
		return mp
	}
	return cervomodelpolicy.NewPolicy(pol)
}

// getPolicy returns the current model-policy config under a read lock. The
// returned value shares the (immutable-after-swap) maps, so it is safe to read
// even while a concurrent ReloadModelPolicy swaps in a new config.
func (s *RouterService) getPolicy() policyConfig {
	s.policyMu.RLock()
	defer s.policyMu.RUnlock()
	return s.policy
}

func (s *RouterService) setModelPolicyConfig(policy policyConfig) {
	normalized := cervomodelpolicy.NormalizeConfig(policy)
	mp := cervomodelpolicy.NewPolicy(normalized)
	s.policyMu.Lock()
	s.policy = normalized
	s.modelPolicy = mp
	s.policyMu.Unlock()
}

// ReloadModelPolicy re-reads the model policy (embedded default < model-policy.json
// < env) and atomically swaps it in, so free-model chains can be updated without
// a full restart. Refuses to swap in a policy that fails strict validation.
func (s *RouterService) ReloadModelPolicy() error {
	runtime := loadModelPolicyRuntime()
	if runtime.Strict && len(runtime.Warnings) > 0 {
		return fmt.Errorf("model policy strict validation failed (%d warnings); keeping current policy", len(runtime.Warnings))
	}
	s.setModelPolicyConfig(runtime.Config)
	s.policyMu.Lock()
	s.modelWarnings = runtime.Warnings
	s.modelStrict = runtime.Strict
	s.policyMu.Unlock()
	return nil
}
