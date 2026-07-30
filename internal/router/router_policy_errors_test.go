package router

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cervorules "github.com/cervantesh/cervo-rules/v3/core"
	cervoruntime "github.com/cervantesh/cervo-rules/v3/runtime"
)

func TestBuildPolicyFlowReturnsStructuredPolicyBuildError(t *testing.T) {
	_, err := buildPolicyFlow(ruleRuntimeConfig{
		PolicyRuntimeConfig: runtimeConfig(providerOpenRouter, map[cervorules.Operation]cervorules.Target{
			capPlanning: cervorules.Target("unknown_backend"),
		}),
	})
	if err == nil {
		t.Fatal("expected unknown backend to fail vocabulary validation")
	}

	var buildErr *cervoruntime.PolicyBuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("expected PolicyBuildError, got %T: %v", err, err)
	}
	if buildErr.Metadata.Name != "cervoproxy.v3" {
		t.Fatalf("expected generated policy metadata, got %#v", buildErr.Metadata)
	}

	var structured cervorules.Errors
	if !errors.As(err, &structured) {
		t.Fatalf("expected structured CervoRules errors, got %T", err)
	}
	if !structured.Has(cervorules.ErrorCodeUnknownTarget) {
		t.Fatalf("expected unknown target code, got %#v", structured)
	}
	if got := structured.ByCode(cervorules.ErrorCodeUnknownTarget)[0].Field; got != "operation_targets[planning].target" {
		t.Fatalf("expected stable runtime field path, got %q", got)
	}
}

func TestPolicyErrorLogAttrsRedactStructuredValues(t *testing.T) {
	err := cervoruntime.NewPolicyBuildError(cervoruntime.PolicyMetadata{
		Name:       "cervoproxy.v3",
		PolicyHash: "policy-hash",
	}, cervorules.Errors{{
		Code:      cervorules.ErrorCodeInvalidRuntimeConfig,
		Severity:  cervorules.SeverityFatal,
		Component: "runtime",
		Field:     "authorization_header",
		Value:     "Bearer secret-token",
		Reason:    "invalid authorization value",
	}})

	attrs := policyErrorLogAttrs(err)
	got := attrsAsString(attrs)
	if strings.Contains(got, "secret-token") || strings.Contains(got, "Bearer") {
		t.Fatalf("structured error log attrs leaked sensitive value: %s", got)
	}
	for _, want := range []string{"policy_build", "cervoproxy.v3", "invalid_runtime_config", "authorization_header"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected log attrs to contain %q, got %s", want, got)
		}
	}
}

func TestAuthorizeOperationalRouteReturnsGenericErrorForPolicyFailure(t *testing.T) {
	svc := NewRouterService()
	svc.runtimeConfig = normalizeRuleConfig(ruleRuntimeConfig{})
	svc.PolicyEngine = failingPolicyEngine{err: cervorules.Errors{{
		Code:      cervorules.ErrorCodeEvaluationFailed,
		Severity:  cervorules.SeverityFatal,
		Component: "core",
		Field:     "prompt_content",
		Value:     "user secret prompt",
		Reason:    "policy evaluation failed",
	}}}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	rec := httptest.NewRecorder()

	_, ok := svc.authorizeOperationalRoute(context.Background(), rec, req, requestFacts{
		ID:            "req-error",
		OperationHint: capChatCompletion,
	}, []byte(`{"messages":[]}`))

	if ok {
		t.Fatal("expected policy engine error to reject request")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 status, got %d body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "policy decision failed") || strings.Contains(body, "user secret prompt") {
		t.Fatalf("expected generic redacted response body, got %s", body)
	}
}

func attrsAsString(attrs []slog.Attr) string {
	var b strings.Builder
	for _, attr := range attrs {
		b.WriteString(attr.Key)
		b.WriteByte('=')
		b.WriteString(attr.Value.String())
		b.WriteByte('\n')
	}
	return b.String()
}

type failingPolicyEngine struct {
	err error
}

func (e failingPolicyEngine) Decide(ctx context.Context, req cervorules.Request) (cervorules.DecisionResult, error) {
	return e.DecideWithOptions(ctx, req, cervorules.DecisionOptions{})
}

func (e failingPolicyEngine) DecideWithOptions(_ context.Context, req cervorules.Request, options cervorules.DecisionOptions) (cervorules.DecisionResult, error) {
	return cervorules.NewDecisionResult(req, cervorules.Decision{}, cervorules.WithTrace(options.TraceEnabled()), cervorules.WithObservation(options.ObservationEnabled())), e.err
}
