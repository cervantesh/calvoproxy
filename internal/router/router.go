package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	cervomodelpolicy "github.com/cervantesh/cervo-model-policy"
)

func NewRouterService() *RouterService {
	// Accept PROXY_* names for the settings still read under the legacy
	// CERVO_* prefix, before anything (including the vendored policy
	// library) reads them. See bridgeLegacyPolicyEnv.
	bridgeLegacyPolicyEnv()
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
	svc := &RouterService{
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
		admission:      newAdmissionControl(),
		capabilities:   newCapabilityIndex(loadCapabilityOverrides()),
	}
	// Best-effort background auto-derive of model capabilities from OpenRouter;
	// the manual overrides above already cover the chain models synchronously.
	// The refresh goroutine is tied to a service-scoped context so Close() stops
	// it (it used to run off context.Background(), i.e. forever — each new
	// RouterService leaked a ticker).
	refreshCtx, cancelRefresh := context.WithCancel(context.Background())
	svc.cancelRefresh = cancelRefresh
	svc.capabilities.startCapabilityRefresh(refreshCtx)
	return svc
}

// Close releases the service's background workers and, when score persistence
// is running, flushes the learned scores one last time so a clean shutdown
// never loses the tail of what the process learned. Safe to call more than once.
func (s *RouterService) Close() {
	if s.cancelRefresh != nil {
		s.cancelRefresh()
	}
	if s.persistScores.Load() {
		if err := s.SaveScores(); err != nil {
			slog.Warn("[CalvoProxy] could not persist model scores on shutdown", slog.String("error", err.Error()))
		}
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

	// Admission control: cap concurrent in-flight requests (PROXY_MAX_CONCURRENT)
	// so a burst waits briefly instead of stampeding the upstream past its rate
	// limits and collapsing the whole chain to 503. Disabled by default.
	if release, ok := s.admission.acquire(ctx); ok {
		defer release()
	} else {
		s.counters.admissionRejected.Add(1)
		w.Header().Set("Retry-After", strconv.Itoa(s.admission.retryAfterSeconds()))
		writeJSONError(w, http.StatusServiceUnavailable, "Server at capacity (PROXY_MAX_CONCURRENT). Retry shortly.")
		return
	}

	if strings.Contains(r.URL.Path, "embeddings") {
		// This proxy exists to route FREE models, and /v1/embeddings cannot honor
		// that: OpenRouter publishes no free embedding model — none appear in its
		// catalogue at all — so every request here bills real credit (measured:
		// text-embedding-3-small reports cost 4e-08 per call). It is also the one
		// endpoint with no chain, breaker or fallback behind it.
		//
		// Refusing by default keeps the product's promise checkable: an account
		// that never opted in cannot be billed by a path it did not know existed.
		// Opt in with PROXY_ALLOW_PAID_EMBEDDINGS=true.
		if !envBool("PROXY_ALLOW_PAID_EMBEDDINGS", false) {
			s.counters.paidEmbeddingRefused.Add(1)
			writeJSONError(w, http.StatusPaymentRequired,
				"/v1/embeddings is disabled: OpenRouter has no free embedding model, so this endpoint "+
					"spends real credit and is not covered by the model chain, breaker or fallback. "+
					"Set PROXY_ALLOW_PAID_EMBEDDINGS=true to allow it.")
			return
		}
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
		// Route the Anthropic /messages request through the SAME model chain,
		// breaker, scoring and multi-model fallback as chat — each attempt targets
		// the upstream /messages endpoint. If the body isn't routable JSON, fall
		// back to a dumb pass-through tunnel (still authorized). NOTE: this targets
		// OpenRouter/Anthropic /messages; other providers don't expose that shape.
		var msgBody map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &msgBody); err != nil {
			if _, ok := s.authorizeOperationalRoute(ctx, w, r, requestFacts{OperationHint: capChatCompletion}, bodyBytes); !ok {
				return
			}
			slog.InfoContext(ctx, "[CalvoProxy] 🚀 Anthropic passthrough (unroutable body)")
			s.tunnelToOpenRouterMessages(ctx, w, bodyBytes, apiKey)
			return
		}
		messagesRaw, _ := msgBody["messages"].([]interface{})
		hasTools := hasRequestTools(msgBody)
		hasImages := hasImageContent(messagesRaw)
		category := s.determineProfile(r, messagesRaw, provider, hasTools)
		requestedModel, _ := msgBody["model"].(string)
		if s.rejectUnknownProfile(w, requestedModel) {
			return
		}
		category, requestedModel = s.resolveModelAlias(category, requestedModel)
		stream, _ := msgBody["stream"].(bool)
		// Same policy facts as chat, so stream-deny / limits / metadata are
		// enforced identically on the messages path.
		policyFacts := requestPolicyFacts(msgBody, category, requestedModel, stream, hasTools, hasImages, int64(len(bodyBytes)))
		decision, ok := s.authorizeOperationalRoute(ctx, w, r, requestFacts{
			OperationHint: capChatCompletion,
			Stream:        stream,
			Metadata:      policyFacts.Metadata,
		}, bodyBytes, policyFacts.RequestedLimits)
		if !ok {
			return
		}
		slog.InfoContext(ctx, "[CalvoProxy] 🚀 Anthropic /messages via model chain")
		s.dispatchChain(ctx, w, decision, msgBody, apiKey, category, requestedModel, stream, messagesPath, capsRequired(hasImages, hasTools))
		return
	}

	var reqBody map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &reqBody); err != nil {
		slog.WarnContext(ctx, "[CalvoProxy] Invalid JSON received in request body")
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	messagesRaw, _ := reqBody["messages"].([]interface{})
	hasTools := hasRequestTools(reqBody)
	hasImages := hasImageContent(messagesRaw)
	category := s.determineProfile(r, messagesRaw, provider, hasTools)
	requestedModel, _ := reqBody["model"].(string)
	if s.rejectUnknownProfile(w, requestedModel) {
		return
	}
	category, requestedModel = s.resolveModelAlias(category, requestedModel)
	stream, _ := reqBody["stream"].(bool)
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
	s.dispatchChain(ctx, w, decision, reqBody, apiKey, category, requestedModel, stream, "", capsRequired(hasImages, hasTools))
}

// dispatchChain runs a request through the model chain: plan → filter (breaker) →
// rank (score) → truncate → fallback. Shared by /chat/completions and /messages;
// opPath is "" for chat (default) or messagesPath to send each attempt to the
// Anthropic /messages endpoint with the same resilience machinery.
func (s *RouterService) dispatchChain(ctx context.Context, w http.ResponseWriter, decision policyDecision, reqBody map[string]interface{}, apiKey, category, requestedModel string, stream bool, opPath string, required []string) {
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
	// Capability gate: if the request needs vision/tools and the caller pinned a
	// specific model that can't do it, fail with a clear client error rather than
	// routing to it and getting a silent breakage upstream.
	if len(required) > 0 && requestedModel != "" && !strings.EqualFold(strings.TrimSpace(requestedModel), "auto") {
		if !s.capabilities.satisfies(requestedModel, required) {
			s.counters.capabilityRefused.Add(1)
			writeJSONError(w, http.StatusUnprocessableEntity, "requested model "+requestedModel+" does not support "+strings.Join(required, "+"))
			return
		}
	}
	attemptsToTry := s.planModelAttempts(decision, category, requestedModel)
	if opPath != "" {
		for i := range attemptsToTry {
			attemptsToTry[i].Path = opPath
		}
	}
	// Keep only models that support the required capabilities (fail-closed:
	// unknown models don't qualify). Empties → capability rescue across profiles;
	// still empty → a clear capability 503 (distinct from the breaker one).
	if len(required) > 0 {
		attemptsToTry = s.applyCapabilityFilter(attemptsToTry, category, opPath, required)
		if len(attemptsToTry) == 0 {
			s.counters.capabilityRefused.Add(1)
			writeJSONError(w, http.StatusServiceUnavailable, "No available model supports "+strings.Join(required, "+")+" for this request.")
			return
		}
	}
	availableModels := s.filterAvailableAttempts(attemptsToTry)
	// Reorder the breaker-eligible chain by reliability score (most reliable
	// first) before truncating to MaxAttempts, so flaky models sink to the back.
	availableModels = s.rankAttemptsByScore(availableModels)
	if decision.RetryPolicy.MaxAttempts > 0 && len(availableModels) > decision.RetryPolicy.MaxAttempts {
		availableModels = availableModels[:decision.RetryPolicy.MaxAttempts]
	}

	slog.InfoContext(ctx, "[CalvoProxy] 🏷️ Resolving Route",
		slog.String("category", category),
		slog.String("op_path", opPath),
		slog.String("policy_target", string(decision.Target)),
		slog.String("executor", string(decision.Executor)),
		slog.String("rule_id", decision.RuleID),
		slog.String("audit_class", string(decision.AuditClass)),
		slog.String("policy_reason", decision.Reason),
		slog.Any("availableRoute", availableModels),
	)

	if len(availableModels) == 0 {
		// Tell the client WHEN to come back: the soonest cooldown expiry across
		// the planned chain. Without this, clients retry immediately and amplify
		// the outage they're already suffering.
		if wait := s.retryAfterForAttempts(attemptsToTry); wait > 0 {
			secs := int(wait.Seconds())
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
		}
		writeJSONError(w, http.StatusServiceUnavailable, "All models are temporarily rate-limited or unhealthy. Cooling down before retry.")
		return
	}

	perAttempt := s.config.RequestTimeout
	if decision.Timeout > 0 && decision.Timeout < perAttempt {
		perAttempt = decision.Timeout
	}
	err := s.executeFallbacks(ctx, w, FallbackExecution{
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

// applyCapabilityFilter keeps only attempts whose model is KNOWN to support all
// required caps. If that empties the chain (the selected profile has no capable
// model), it falls back to a capability rescue across profiles.
func (s *RouterService) applyCapabilityFilter(attempts []modelAttempt, category, opPath string, required []string) []modelAttempt {
	capable := make([]modelAttempt, 0, len(attempts))
	for _, a := range attempts {
		if s.capabilities.satisfies(a.Model, required) {
			capable = append(capable, a)
		} else if !s.capabilities.known(a.Model) {
			s.capabilities.warnUnknownOnce(a.Model)
		}
	}
	if len(capable) > 0 {
		return capable
	}
	return s.capabilityRescue(category, opPath, required)
}

// capabilityRescue builds a candidate list from the models in the CONFIGURED
// profiles (the curated free chains) that satisfy the required caps — never the
// full OpenRouter catalog, so rescue can't escape to paid/unvetted models.
// Deduped by model, stable (sorted) order, stamped with the request's profile so
// breaker/scoring/audit keep the caller's intent.
func (s *RouterService) capabilityRescue(category, opPath string, required []string) []modelAttempt {
	seen := map[string]bool{}
	models := make([]string, 0)
	for _, chain := range s.getPolicy().Profiles {
		for _, m := range chain {
			if seen[m] {
				continue
			}
			seen[m] = true
			if s.capabilities.satisfies(m, required) {
				models = append(models, m)
			}
		}
	}
	if len(models) == 0 {
		return nil
	}
	sort.Strings(models)
	provider := s.defaultPolicyProvider()
	breaker := s.defaultBreakerPolicy()
	out := make([]modelAttempt, 0, len(models))
	for _, m := range models {
		out = append(out, modelAttempt{Profile: category, Model: m, Provider: provider, BreakerPolicy: breaker, Path: opPath})
	}
	return out
}

func (s *RouterService) determineProfile(r *http.Request, messages []interface{}, forcedProfile string, hasTools bool) string {
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
	// Auto-switch to the curated vision chain for image requests.
	//
	// This used to be skipped whenever tools were also required, on the
	// assumption that the vision chain held no tool-capable model. That is no
	// longer true — every model in it declares both — and the assumption was
	// expensive: an agent sends tools on essentially every turn, so it never
	// took this path and always fell to the capability rescue, which picks any
	// model satisfying the requirement rather than the curated best. In practice
	// that meant every image an agent ever saw was answered by the weakest
	// vision-capable model, invisibly.
	//
	// So switch whenever the chain can actually serve the request: checked
	// against the capability index rather than assumed, which keeps the gate
	// fail-closed if the chain is ever edited down to vision-only models.
	if s.hasImageContent(messages) {
		if chain, ok := pol.Profiles["vision"]; ok && s.visionChainServes(chain, hasTools) {
			selected = "vision"
		}
	}
	if _, ok := pol.Profiles[selected]; !ok {
		return pol.DefaultProfile
	}
	return selected
}

// visionChainServes reports whether the vision chain can plausibly serve this
// request. With tools required that means a model declaring both capabilities;
// without, vision alone is enough.
//
// A model the index has never heard of counts as usable. The chain is curated —
// an operator put those models there deliberately — so an index that simply has
// no data yet (auto-derive has not run, or the model is new) must not override
// that configuration and strand image requests on a text-only chain. The
// capability filter downstream is fail-closed and still has the final say; this
// check only avoids switching into a chain KNOWN to be unable to serve.
func (s *RouterService) visionChainServes(chain []string, hasTools bool) bool {
	required := []string{"vision"}
	if hasTools {
		required = append(required, "tools")
	}
	for _, model := range chain {
		if !s.capabilities.known(model) || s.capabilities.satisfies(model, required) {
			return true
		}
	}
	return false
}

// rejectUnknownProfile reports whether an explicitly requested model name names
// no profile and no alias — and, when strict naming is enabled, answers 404.
//
// Why this exists: an unknown name is otherwise silently ignored and the profile
// is chosen by keyword-classifying the prompt, so a request for a profile that
// does not exist (a typo, or a client written against docs that landed before
// the policy did) returns 200 from whatever chain classification picked. The
// caller believes it got the profile it named. For a fail-closed profile that
// guarantee is the entire feature.
//
// OFF BY DEFAULT (PROXY_STRICT_PROFILE_NAMES), because rejecting is wrong for an
// OpenAI-compatible proxy: clients legitimately send their own model names. This
// shipped hard-on in v0.5.0 and broke a real client — Hermes calls its model
// "Sol", so every turn that named it got a 404 while requests naming "coding"
// sailed through, which is exactly the intermittent, hard-to-attribute failure a
// gate like this should never cause.
//
// The counter is incremented either way, so a substitution is still visible in
// /metrics even when it is allowed to proceed.
//
// Scoped deliberately to names WITHOUT a "/": every real upstream model id is
// `vendor/model`, so pinning a specific model — including one absent from every
// chain — keeps working untouched.
func (s *RouterService) rejectUnknownProfile(w http.ResponseWriter, requestedModel string) bool {
	name := strings.TrimSpace(requestedModel)
	if name == "" || strings.Contains(name, "/") {
		return false
	}
	// "auto" is the conventional sentinel for "you choose" — it names no
	// profile on purpose, so rejecting it would break every client that uses it
	// to defer the decision to the proxy. (Caught by the integration tests,
	// which is precisely what they are for.)
	if strings.EqualFold(name, "auto") {
		return false
	}
	if _, ok := s.resolveProfileAlias(name); ok {
		return false
	}
	pol := s.getPolicy()
	if _, ok := pol.Profiles[name]; ok {
		return false
	}
	// A model configured in any chain is a legitimate pin, even when its id has
	// no vendor prefix — the "/" test above is a shortcut for ids this proxy has
	// never seen, not the definition of a model. (The capability tests pin a
	// bare `model-notools`, which must reach the 422 incapable-pin path rather
	// than being turned away here as if it were a typo.)
	for _, chain := range pol.Profiles {
		for _, model := range chain {
			if strings.EqualFold(strings.TrimSpace(model), name) {
				return false
			}
		}
	}
	// Unknown either way — count it so the substitution is never invisible.
	s.counters.unknownProfileRejected.Add(1)
	if !envBool("PROXY_STRICT_PROFILE_NAMES", false) {
		slog.Debug("[CalvoProxy] unknown profile name, falling back to classification",
			slog.String("requested", name))
		return false
	}
	known := profileNames(pol.Profiles)
	writeJSONError(w, http.StatusNotFound, fmt.Sprintf(
		"unknown profile %q; known profiles: %s (pin a specific model with its full vendor/model id)",
		name, strings.Join(known, ", ")))
	return true
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
	// Reload capability overrides in the same pass, so an edit to the chains and
	// their per-model capabilities stays consistent (auto-derived data is kept).
	if s.capabilities != nil {
		s.capabilities.setOverrides(loadCapabilityOverrides())
	}
	return nil
}
