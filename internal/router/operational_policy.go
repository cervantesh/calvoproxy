package router

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	cervorules "github.com/cervantesh/cervo-rules/v3/core"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

const (
	headerCapability = "X-Cervo-Capability"
	headerUser       = "X-Cervo-User"
	headerChannel    = "X-Cervo-Channel"
	headerRisk       = "X-Cervo-Risk"
	headerRequestID  = "X-Request-Id"
)

func (s *RouterService) authorizeOperationalRoute(ctx context.Context, w http.ResponseWriter, r *http.Request, facts requestFacts, body []byte, requestedLimits ...RequestedLimits) (policyDecision, bool) {
	if s.PolicyEngine == nil {
		decision := defaultAllowedDecision()
		decision.Timeout = s.config.RequestTimeout
		return decision, true
	}

	httpFacts := proxyHTTPClassifier.FactsFromHTTPRequest(r)
	facts = mergeRequestFacts(httpFacts, facts)
	req := requestFromFacts(facts)
	if req.Metadata == nil {
		req.Metadata = map[string]string{}
	}
	if len(body) > 0 {
		req.Metadata["body_bytes"] = strconv.Itoa(len(body))
	}
	requested := requestedPolicyLimits(facts, body, requestedLimits...)
	health := s.Health()
	derived := derivePolicyFacts(facts, policyRequestFacts{Metadata: req.Metadata, RequestedLimits: requested}, health)
	if health.Status != "" {
		req.Metadata["proxy_status"] = health.Status
		req.Metadata["proxy_ready"] = strconv.FormatBool(health.Ready)
	}
	if health.OpenCircuitCount > 0 {
		req.Metadata["proxy_circuit_state"] = "has_open_circuits"
	}
	req.Metadata["derived_policy_fact_count"] = strconv.Itoa(len(derived.Facts))

	telemetryConfig := loadPolicyTelemetryConfig()
	start := time.Now()
	policyCtx, span := otel.Tracer("cervoproxy/policy").Start(ctx, "cervoproxy.policy.evaluate")
	defer span.End()
	result, err := s.PolicyEngine.DecideWithOptions(policyCtx, req, policyDecisionOptionsForRequest(req, telemetryConfig))
	decision := s.proxyDecision(req, result.Decision)
	event := newPolicyTelemetryEventFromResult(req, result, decision, s.policyMetadata, err, time.Since(start))
	recordPolicyTelemetry(policyCtx, event, telemetryConfig, span)
	if err != nil {
		span.SetStatus(codes.Error, "policy decision failed")
		if telemetryConfig.LogEnabled {
			attrs := append([]slog.Attr{slog.String("request_id", req.ID)}, event.LogAttrs()...)
			slog.LogAttrs(policyCtx, slog.LevelError, "[CervoProxy] policy decision failed", attrs...)
		}
		writeJSONError(w, http.StatusInternalServerError, "policy decision failed")
		return decision, false
	}

	if telemetryConfig.LogEnabled {
		level := telemetryConfig.LogLevel
		if !decision.Allow && decision.RequiresAudit && level < slog.LevelWarn {
			level = slog.LevelWarn
		}
		slog.LogAttrs(policyCtx, level, "[CervoProxy] policy decision", event.LogAttrs()...)
	}

	if !decision.Allow {
		writeJSONError(w, http.StatusForbidden, decision.Reason)
		return decision, false
	}
	if !validateDecisionLimits(w, decision, requested) {
		return decision, false
	}
	return decision, true
}

func (s *RouterService) proxyDecision(req cervorules.Request, decision cervorules.Decision) policyDecision {
	out := policyDecision{
		Allow:             decision.Allow,
		Target:            decision.Target,
		Executor:          decision.Executor,
		FallbackExecutors: append([]cervorules.Executor(nil), decision.FallbackExecutors...),
		Reason:            decision.Reason,
		RuleID:            "route." + string(req.Operation),
		AuditClass:        auditClassForRequest(req),
		RequiresAudit:     auditClassForRequest(req) != "none",
		Timeout:           s.runtimeConfig.DefaultTimeout,
		RetryPolicy:       s.runtimeConfig.RetryPolicy,
		BreakerPolicy:     s.runtimeConfig.BreakerPolicy,
		Limits:            limitsForOperation(req.Operation, s.runtimeConfig.Limits),
	}
	if !out.Allow {
		return out
	}
	if out.Executor == "" {
		out.Executor = s.defaultPolicyProvider()
	}
	if len(out.FallbackExecutors) == 0 {
		fallbacks := s.runtimeConfig.ExecutorFallbacks[out.Executor]
		out.FallbackExecutors = append([]cervorules.Executor(nil), fallbacks...)
	}
	if req.Operation == capSecretLookup {
		out.RuleID = "secret.route"
		out.AuditClass = "sensitive"
		out.RequiresAudit = true
	}
	return out
}
