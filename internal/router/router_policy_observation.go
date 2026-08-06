package router

import (
	"context"
	"errors"
	"hash/fnv"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	cervorules "github.com/cervantesh/cervo-rules/v3/core"
	cervoobserve "github.com/cervantesh/cervo-rules/v3/observe"
	cervoruntime "github.com/cervantesh/cervo-rules/v3/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const policyTelemetrySchemaVersion = "calvoproxy.policy_telemetry.v1"

type PolicyTelemetryEvent struct {
	SchemaVersion string
	PolicyName    string
	PolicyHash    string
	Operation     string
	Target        string
	Executor      string
	Decision      string
	RuleID        string
	AuditClass    string
	RequiresAudit bool
	ProxyReady    string
	ProxyStatus   string
	ErrorKind     string
	ErrorCodes    []string
	ErrorFields   []string
	DurationMS    int64
}

type policyTelemetryConfig struct {
	LogEnabled            bool
	MetricsEnabled        bool
	TraceEnabled          bool
	ObservationSampleRate float64
	DebugIncludeTrace     bool
	LogLevel              slog.Level
}

var (
	policyMetricsOnce       sync.Once
	policyDecisionCounter   metric.Int64Counter
	policyErrorCounter      metric.Int64Counter
	policyDecisionDuration  metric.Float64Histogram
	policyMetricInstruments bool
)

func policyDecisionObservationFields(req cervorules.Request, decision policyDecision) map[string]string {
	fields := map[string]string{
		"operation":      string(req.Operation),
		"target":         string(decision.Target),
		"executor":       string(decision.Executor),
		"reason":         decision.Reason,
		"rule_id":        decision.RuleID,
		"audit_class":    decision.AuditClass,
		"requires_audit": strconv.FormatBool(decision.RequiresAudit),
	}
	for _, key := range []string{"proxy_ready", "proxy_status", "proxy_circuit_state"} {
		if value := req.Metadata[key]; value != "" {
			fields[key] = value
		}
	}
	return fields
}

func decisionLogAttrs(req cervorules.Request, decision policyDecision) []any {
	event := newPolicyTelemetryEvent(req, decision, generatedPolicyMetadata(), nil, 0)
	attrs := event.LogAttrs()
	out := make([]any, 0, len(attrs))
	for _, attr := range attrs {
		out = append(out, attr)
	}
	return out
}

func newPolicyTelemetryEvent(req cervorules.Request, decision policyDecision, metadata cervoruntime.PolicyMetadata, err error, duration time.Duration) PolicyTelemetryEvent {
	if metadata.Name == "" {
		metadata = generatedPolicyMetadata()
	}
	fields := policyDecisionObservationFields(req, decision)
	event := PolicyTelemetryEvent{
		SchemaVersion: policyTelemetrySchemaVersion,
		PolicyName:    valueOrNone(metadata.Name),
		PolicyHash:    valueOrNone(metadata.PolicyHash),
		Operation:     valueOrNone(fields["operation"]),
		Target:        valueOrNone(fields["target"]),
		Executor:      valueOrNone(fields["executor"]),
		Decision:      policyDecisionOutcome(decision, err),
		RuleID:        fields["rule_id"],
		AuditClass:    fields["audit_class"],
		RequiresAudit: decision.RequiresAudit,
		ProxyReady:    fields["proxy_ready"],
		ProxyStatus:   fields["proxy_status"],
		DurationMS:    duration.Milliseconds(),
	}
	if err != nil {
		event.ErrorKind = policyErrorKind(err)
		event.ErrorCodes, event.ErrorFields = policyStructuredErrorValues(err)
	}
	return event
}

func newPolicyTelemetryEventFromResult(req cervorules.Request, result cervorules.DecisionResult, decision policyDecision, metadata cervoruntime.PolicyMetadata, err error, duration time.Duration) PolicyTelemetryEvent {
	event := newPolicyTelemetryEvent(req, decision, metadata, err, duration)
	if result.Observation != nil {
		report := cervoobserve.NewPolicyEvaluationReport(result)
		if report.Operation != "" {
			event.Operation = valueOrNone(string(report.Operation))
		}
		if report.Target != "" {
			event.Target = valueOrNone(string(report.Target))
		}
		if report.Executor != "" {
			event.Executor = valueOrNone(string(report.Executor))
		}
	}
	return event
}

func (e PolicyTelemetryEvent) LogAttrs() []slog.Attr {
	attrs := []slog.Attr{
		slog.String("event", "cervorules.policy.decision"),
		slog.String("schema_version", e.SchemaVersion),
		slog.String("policy_name", e.PolicyName),
		slog.String("policy_hash", e.PolicyHash),
		slog.String("operation", e.Operation),
		slog.String("target", e.Target),
		slog.String("executor", e.Executor),
		slog.String("decision", e.Decision),
		slog.String("rule_id", e.RuleID),
		slog.String("audit_class", e.AuditClass),
		slog.Bool("requires_audit", e.RequiresAudit),
		slog.Int64("duration_ms", e.DurationMS),
	}
	if e.ProxyReady != "" {
		attrs = append(attrs, slog.String("proxy_ready", e.ProxyReady))
	}
	if e.ProxyStatus != "" {
		attrs = append(attrs, slog.String("proxy_status", e.ProxyStatus))
	}
	if e.ErrorKind != "" {
		attrs = append(attrs, slog.String("error_kind", e.ErrorKind))
	}
	if len(e.ErrorCodes) > 0 {
		attrs = append(attrs, slog.String("error_codes", strings.Join(e.ErrorCodes, ",")))
	}
	if len(e.ErrorFields) > 0 {
		attrs = append(attrs, slog.String("error_fields", strings.Join(e.ErrorFields, ",")))
	}
	return attrs
}

func (e PolicyTelemetryEvent) MetricLabels() map[string]string {
	return map[string]string{
		"policy_name": e.PolicyName,
		"operation":   valueOrNone(e.Operation),
		"target":      valueOrNone(e.Target),
		"executor":    valueOrNone(e.Executor),
		"decision":    valueOrNone(e.Decision),
	}
}

func (e PolicyTelemetryEvent) ErrorMetricLabels() map[string]string {
	code := "none"
	if len(e.ErrorCodes) == 1 {
		code = e.ErrorCodes[0]
	} else if len(e.ErrorCodes) > 1 {
		code = "multiple"
	}
	return map[string]string{
		"policy_name": e.PolicyName,
		"operation":   valueOrNone(e.Operation),
		"error_code":  code,
	}
}

func (e PolicyTelemetryEvent) TraceAttributes() []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("cervo.policy.name", e.PolicyName),
		attribute.String("cervo.policy.hash", e.PolicyHash),
		attribute.String("cervo.policy.operation", valueOrNone(e.Operation)),
		attribute.String("cervo.policy.target", valueOrNone(e.Target)),
		attribute.String("cervo.policy.executor", valueOrNone(e.Executor)),
		attribute.String("cervo.policy.decision", valueOrNone(e.Decision)),
		attribute.Bool("cervo.policy.requires_audit", e.RequiresAudit),
	}
	if len(e.ErrorCodes) > 0 {
		attrs = append(attrs, attribute.String("cervo.policy.error_codes", strings.Join(e.ErrorCodes, ",")))
	}
	return attrs
}

func recordPolicyTelemetry(ctx context.Context, event PolicyTelemetryEvent, cfg policyTelemetryConfig, span trace.Span) {
	if cfg.TraceEnabled && span != nil {
		span.SetAttributes(event.TraceAttributes()...)
	}
	if cfg.MetricsEnabled {
		recordPolicyMetrics(ctx, event)
	}
}

func recordPolicyMetrics(ctx context.Context, event PolicyTelemetryEvent) {
	policyMetricsOnce.Do(func() {
		meter := otel.Meter("calvoproxy/policy")
		var err error
		policyDecisionCounter, err = meter.Int64Counter("calvoproxy_policy_decisions_total")
		if err != nil {
			return
		}
		policyErrorCounter, err = meter.Int64Counter("calvoproxy_policy_errors_total")
		if err != nil {
			return
		}
		policyDecisionDuration, err = meter.Float64Histogram("calvoproxy_policy_decision_duration_ms")
		if err != nil {
			return
		}
		policyMetricInstruments = true
	})
	if !policyMetricInstruments {
		return
	}
	policyDecisionCounter.Add(ctx, 1, metric.WithAttributes(attributesFromLabels(event.MetricLabels())...))
	policyDecisionDuration.Record(ctx, float64(event.DurationMS), metric.WithAttributes(attributesFromLabels(event.MetricLabels())...))
	if event.Decision == "error" {
		policyErrorCounter.Add(ctx, 1, metric.WithAttributes(attributesFromLabels(event.ErrorMetricLabels())...))
	}
}

func attributesFromLabels(labels map[string]string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(labels))
	for _, key := range []string{"policy_name", "operation", "target", "executor", "decision", "error_code"} {
		if value, ok := labels[key]; ok {
			attrs = append(attrs, attribute.String(key, valueOrNone(value)))
		}
	}
	return attrs
}

func loadPolicyTelemetryConfig() policyTelemetryConfig {
	return policyTelemetryConfig{
		LogEnabled:            envBool("CERVO_POLICY_LOG_ENABLED", true),
		MetricsEnabled:        envBool("CERVO_POLICY_METRICS_ENABLED", true),
		TraceEnabled:          envBool("CERVO_POLICY_TRACE_ENABLED", true),
		ObservationSampleRate: clampPolicySampleRate(envFloat("CERVO_POLICY_OBSERVATION_SAMPLE_RATE", 0)),
		DebugIncludeTrace:     envBool("CERVO_POLICY_DEBUG_INCLUDE_TRACE", false),
		LogLevel:              policyLogLevelFromEnv(),
	}
}

// policyDecisionOptionsForRequest decides what the engine is asked to produce
// beyond the decision itself.
//
// routeTraceOn is passed in rather than read here so this stays a pure function
// of its inputs, and so the ONE place that reads PROXY_ROUTE_TRACE for this
// purpose is the call site next to the engine call. It only widens the trace
// option, never the observation one: the observation feeds metrics and is
// sampled deliberately, whereas the trace's cost is a handful of steps that die
// with the request.
//
// The route trace is on by default, so on a default install this does ask the
// engine for a trace on every request — one slice of at most one struct per
// policy rule, all of it garbage by the end of the request. PROXY_ROUTE_TRACE=off
// takes it back to nothing, which is the guarantee spec §7 makes.
func policyDecisionOptionsForRequest(req cervorules.Request, cfg policyTelemetryConfig, routeTraceOn bool) cervorules.DecisionOptions {
	includeDiagnostics := cfg.DebugIncludeTrace || shouldSamplePolicyObservation(req.ID, cfg.ObservationSampleRate)
	return cervorules.NewDecisionOptions(
		cervorules.WithTrace(includeDiagnostics || routeTraceOn),
		cervorules.WithObservation(includeDiagnostics),
	)
}

// policyStepsFromResult flattens the engine's trace onto the router's own type.
// Returns nil when the engine produced no trace, which is what happens whenever
// the route trace is off — so with PROXY_ROUTE_TRACE=off nothing is allocated
// here at all.
func policyStepsFromResult(result cervorules.DecisionResult) []policyTraceStep {
	if result.Trace == nil || len(result.Trace.Steps) == 0 {
		return nil
	}
	steps := make([]policyTraceStep, 0, len(result.Trace.Steps))
	for _, step := range result.Trace.Steps {
		steps = append(steps, policyTraceStep{Name: step.Name, Matched: step.Matched, Detail: step.Reason})
	}
	return steps
}

func shouldSamplePolicyObservation(id string, rate float64) bool {
	if rate <= 0 {
		return false
	}
	if rate >= 1 {
		return true
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return float64(h.Sum32()%10000)/10000 < rate
}

func envFloat(key string, fallback float64) float64 {
	raw := envValue(key)
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func clampPolicySampleRate(rate float64) float64 {
	if math.IsNaN(rate) || rate < 0 {
		return 0
	}
	if rate > 1 {
		return 1
	}
	return rate
}

func policyLogLevelFromEnv() slog.Level {
	switch strings.ToLower(envValue("CERVO_POLICY_LOG_LEVEL")) {
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func policyDecisionOutcome(decision policyDecision, err error) string {
	if err != nil {
		return "error"
	}
	if decision.Allow {
		return "allow"
	}
	return "deny"
}

func policyErrorKind(err error) string {
	var buildErr *cervoruntime.PolicyBuildError
	if errors.As(err, &buildErr) {
		return "policy_build"
	}
	var structured cervorules.Errors
	if errors.As(err, &structured) {
		return "cervorules"
	}
	var single cervorules.Error
	if errors.As(err, &single) {
		return "cervorules"
	}
	return "generic"
}

func policyStructuredErrorValues(err error) ([]string, []string) {
	var buildErr *cervoruntime.PolicyBuildError
	if errors.As(err, &buildErr) {
		return structuredErrorValues(buildErr.Errors)
	}
	var errs cervorules.Errors
	if errors.As(err, &errs) {
		return structuredErrorValues(errs)
	}
	var single cervorules.Error
	if errors.As(err, &single) {
		return structuredErrorValues(cervorules.Errors{single})
	}
	return nil, nil
}

func structuredErrorValues(errs cervorules.Errors) ([]string, []string) {
	errs = errs.Redacted().SortStable()
	codes := make([]string, 0, len(errs))
	fields := make([]string, 0, len(errs))
	for _, err := range errs {
		if err.Code != "" {
			codes = appendUnique(codes, string(err.Code))
		}
		if err.Field != "" {
			fields = appendUnique(fields, err.Field)
		}
	}
	return codes, fields
}

func valueOrNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "none"
	}
	return value
}
