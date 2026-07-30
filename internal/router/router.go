package router

import (
	"context"
	"encoding/json"
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

	transport := &GlobalBreakerTransport{
		Base:             http.DefaultTransport,
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
		Client:         &http.Client{Timeout: config.RequestTimeout, Transport: transport},
		SideEffects:    NewWorkspaceSideEffectExtractor(),
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

	bodyBytes, _ := io.ReadAll(r.Body)

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
		decision, ok := s.authorizeOperationalRoute(ctx, w, r, requestFacts{
			OperationHint: capChatCompletion,
		}, bodyBytes)
		if !ok {
			return
		}
		if decision.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, decision.Timeout)
			defer cancel()
		}
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
		Stream:   stream,
		Metadata: policyFacts.Metadata,
	}, bodyBytes, policyFacts.RequestedLimits)
	if !ok {
		return
	}
	if decision.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, decision.Timeout)
		defer cancel()
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

	err := s.executeFallbacks(ctx, w, FallbackExecution{
		RequestBody: reqBody,
		APIKey:      apiKey,
		Attempts:    availableModels,
		RetryPolicy: decision.RetryPolicy,
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
	if s.hasImageContent(messages) {
		if _, ok := s.policy.Profiles["vision"]; ok {
			selected = "vision"
		}
	}
	if _, ok := s.policy.Profiles[selected]; !ok {
		return s.policy.DefaultProfile
	}
	return selected
}

func (s *RouterService) resolveModelAlias(category string, requestedModel string) (string, string) {
	if reqModelStr := strings.TrimSpace(requestedModel); reqModelStr != "" {
		parts := strings.SplitN(reqModelStr, "/", 2)
		if len(parts) == 2 {
			prefix := strings.ToLower(strings.TrimSpace(parts[0]))
			if prefix == "calvoproxy" || prefix == "cervoclaw" {
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
		planner = PolicyModelAttemptPlanner{Policy: s.policy, model: s.activeModelPolicy()}
	}
	return planner.Plan(decision, category, requestedModel)
}

func (s *RouterService) activeModelPolicy() *cervomodelpolicy.Policy {
	if s.modelPolicy != nil {
		return s.modelPolicy
	}
	return cervomodelpolicy.NewPolicy(s.policy)
}

func (s *RouterService) setModelPolicyConfig(policy policyConfig) {
	s.policy = cervomodelpolicy.NormalizeConfig(policy)
	s.modelPolicy = cervomodelpolicy.NewPolicy(s.policy)
}
