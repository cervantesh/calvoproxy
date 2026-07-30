package router

import (
	"errors"
	"strings"
	"testing"
	"time"

	cervorules "github.com/cervantesh/cervo-rules/v3/core"
	cervoruntime "github.com/cervantesh/cervo-rules/v3/runtime"
	"go.opentelemetry.io/otel/attribute"
)

func TestPolicyDecisionObservationFieldsAreLowCardinality(t *testing.T) {
	req := cervorules.Request{
		ID:        "req-1",
		Operation: capChatCompletion,
		Metadata: map[string]string{
			"profile":         "coding",
			"requested_model": "expensive-model",
			"proxy_ready":     "true",
			"proxy_status":    "ok",
		},
	}
	decision := policyDecision{
		Allow:         true,
		Target:        serviceCervoCore,
		Executor:      providerOpenRouter,
		Reason:        "route matched",
		RuleID:        "route.chat_completion",
		AuditClass:    "standard",
		RequiresAudit: true,
	}

	fields := policyDecisionObservationFields(req, decision)

	for _, key := range []string{"operation", "target", "executor", "reason", "rule_id", "audit_class", "requires_audit", "proxy_ready", "proxy_status"} {
		if fields[key] == "" {
			t.Fatalf("expected %s in observation fields: %+v", key, fields)
		}
	}
	if _, ok := fields["profile"]; ok {
		t.Fatalf("profile must not be emitted as low-cardinality observation field: %+v", fields)
	}
	if _, ok := fields["requested_model"]; ok {
		t.Fatalf("requested_model must not be emitted as low-cardinality observation field: %+v", fields)
	}
}

func TestPolicyTelemetryEventForAllowUsesLowCardinalityFields(t *testing.T) {
	req := cervorules.Request{
		ID:        "req-sensitive",
		User:      "cervantes",
		Operation: capChatCompletion,
		Metadata: map[string]string{
			"profile":              "coding",
			"requested_model":      "model-123",
			"authorization_header": "Bearer secret",
			"prompt_content":       "do not log",
			"proxy_ready":          "true",
		},
	}
	decision := policyDecision{
		Allow:         true,
		Target:        serviceCervoCore,
		Executor:      providerOpenRouter,
		Reason:        "route matched",
		RuleID:        "route.chat_completion",
		AuditClass:    "standard",
		RequiresAudit: true,
	}

	event := newPolicyTelemetryEvent(req, decision, generatedPolicyMetadata(), nil, 12*time.Millisecond)

	if event.Decision != "allow" {
		t.Fatalf("expected allow decision, got %#v", event)
	}
	if event.SchemaVersion == "" || event.PolicyName == "" || event.PolicyHash == "" {
		t.Fatalf("expected versioned policy metadata, got %#v", event)
	}
	labels := event.MetricLabels()
	for _, key := range []string{"policy_name", "operation", "target", "executor", "decision"} {
		if labels[key] == "" {
			t.Fatalf("missing metric label %s in %#v", key, labels)
		}
	}
	for _, forbidden := range []string{"user", "request_id", "reason", "profile", "requested_model", "authorization_header", "prompt_content"} {
		if _, ok := labels[forbidden]; ok {
			t.Fatalf("forbidden metric label %q in %#v", forbidden, labels)
		}
	}
	if got := attrsAsString(event.LogAttrs()); strings.Contains(got, "secret") || strings.Contains(got, "model-123") || strings.Contains(got, "do not log") {
		t.Fatalf("log attrs leaked sensitive request metadata: %s", got)
	}
}

func TestPolicyTelemetryEventForDenyIsNotOperationalError(t *testing.T) {
	req := cervorules.Request{ID: "req-deny", Operation: capMediaRequest}
	decision := policyDecision{
		Allow:      false,
		Target:     serviceCervoMedia,
		Executor:   providerOpenRouter,
		Reason:     "media backend not configured",
		RuleID:     "route.media_request",
		AuditClass: "standard",
	}

	event := newPolicyTelemetryEvent(req, decision, generatedPolicyMetadata(), nil, time.Millisecond)

	if event.Decision != "deny" {
		t.Fatalf("expected deny decision, got %#v", event)
	}
	if len(event.ErrorCodes) != 0 || len(event.ErrorFields) != 0 {
		t.Fatalf("deny must not be reported as error: %#v", event)
	}
	if attrs := event.TraceAttributes(); traceAttrValue(attrs, "cervo.policy.decision") != "deny" {
		t.Fatalf("expected trace deny attr, got %#v", attrs)
	}
}

func TestPolicyTelemetryEventForErrorRedactsAndClassifies(t *testing.T) {
	req := cervorules.Request{
		ID:        "req-error",
		Operation: capPlanning,
		Metadata:  map[string]string{"body": "sensitive body"},
	}
	decision := policyDecision{RuleID: "route.planning"}
	err := cervoruntime.NewPolicyBuildError(cervoruntime.PolicyMetadata{
		Name:       "calvoproxy.v3",
		PolicyHash: "policy-hash",
	}, cervorules.Errors{{
		Code:     cervorules.ErrorCodeInvalidRuntimeConfig,
		Severity: cervorules.SeverityFatal,
		Field:    "authorization_header",
		Value:    "Bearer secret-token",
		Reason:   "invalid authorization value",
	}})

	event := newPolicyTelemetryEvent(req, decision, generatedPolicyMetadata(), err, 3*time.Millisecond)

	if event.Decision != "error" {
		t.Fatalf("expected error decision, got %#v", event)
	}
	if !errors.As(err, new(*cervoruntime.PolicyBuildError)) {
		t.Fatalf("test setup expected policy build error")
	}
	if len(event.ErrorCodes) != 1 || event.ErrorCodes[0] != string(cervorules.ErrorCodeInvalidRuntimeConfig) {
		t.Fatalf("unexpected error codes: %#v", event.ErrorCodes)
	}
	if len(event.ErrorFields) != 1 || event.ErrorFields[0] != "authorization_header" {
		t.Fatalf("unexpected error fields: %#v", event.ErrorFields)
	}
	if got := attrsAsString(event.LogAttrs()); strings.Contains(got, "secret-token") || strings.Contains(got, "Bearer") || strings.Contains(got, "sensitive body") {
		t.Fatalf("error log attrs leaked sensitive values: %s", got)
	}
	errorLabels := event.ErrorMetricLabels()
	if errorLabels["error_code"] != string(cervorules.ErrorCodeInvalidRuntimeConfig) {
		t.Fatalf("expected first error code metric label, got %#v", errorLabels)
	}
	for _, forbidden := range []string{"error_fields", "request_id", "reason", "body"} {
		if _, ok := errorLabels[forbidden]; ok {
			t.Fatalf("forbidden error metric label %q in %#v", forbidden, errorLabels)
		}
	}
}

func TestPolicyTelemetryConfigDefaultsKeepHotPathCheap(t *testing.T) {
	cfg := loadPolicyTelemetryConfig()

	if !cfg.LogEnabled || !cfg.MetricsEnabled || !cfg.TraceEnabled {
		t.Fatalf("expected logs, metrics and lightweight tracing enabled by default: %#v", cfg)
	}
	if cfg.DebugIncludeTrace {
		t.Fatalf("full CervoRules trace must be disabled by default: %#v", cfg)
	}
	if cfg.ObservationSampleRate != 0 {
		t.Fatalf("CervoRules observation materialization must be unsampled by default: %#v", cfg)
	}
}

func traceAttrValue(attrs []attribute.KeyValue, key string) string {
	for _, attr := range attrs {
		if string(attr.Key) == key {
			return attr.Value.AsString()
		}
	}
	return ""
}
