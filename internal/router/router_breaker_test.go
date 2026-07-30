package router

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

type stubTransport struct {
	resp *http.Response
	err  error
}

func (s stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return s.resp, s.err
}

func TestGlobalBreakerTransport_OpensAndResets(t *testing.T) {
	transport := &GlobalBreakerTransport{
		Base:             stubTransport{err: errors.New("boom")},
		FailureThreshold: 1,
		Cooldown:         time.Minute,
	}

	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if _, err := transport.RoundTrip(req); err == nil {
		t.Fatal("expected round trip error")
	}
	if transport.failures != 1 {
		t.Fatalf("expected failures=1, got %d", transport.failures)
	}
	if transport.openUntil.IsZero() {
		t.Fatal("expected transport to open circuit")
	}
	if _, err := transport.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "open") {
		t.Fatalf("expected open circuit error, got %v", err)
	}

	transport.Base = stubTransport{resp: &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}}
	transport.openUntil = time.Time{}
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("unexpected success error: %v", err)
	}
	if transport.failures != 0 {
		t.Fatalf("expected failures reset, got %d", transport.failures)
	}
}

func TestClassifyTransportError_Timeout(t *testing.T) {
	err := classifyTransportError(timeoutErr{})
	if err.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("expected timeout status, got %d", err.StatusCode)
	}
	if !err.BreakerEligible || !err.Retryable {
		t.Fatalf("expected breaker-eligible retryable timeout: %+v", err)
	}
}

func TestHealthIncludesGeneratedPolicyMetadata(t *testing.T) {
	svc := NewRouterService()
	health := svc.Health()

	if health.PolicyName != "calvoproxy.v3" {
		t.Fatalf("expected generated policy name in health, got %+v", health)
	}
	if health.PolicyDSLVersion != "cervorules.policy.v3" {
		t.Fatalf("expected generated policy DSL version in health, got %+v", health)
	}
	if health.PolicyHash == "" || health.PolicyVocabHash == "" {
		t.Fatalf("expected generated policy hashes in health, got %+v", health)
	}
}

func TestClassifyTransportError_GenericNetError(t *testing.T) {
	var netErr net.Error = timeoutErr{}
	err := classifyTransportError(netErr)
	if err == nil || err.StatusCode == 0 {
		t.Fatalf("expected classified error, got %+v", err)
	}
}

func TestClassifyHTTPError_RateLimitAndServerErrors(t *testing.T) {
	rateLimited := classifyHTTPError(http.StatusTooManyRequests, "")
	if rateLimited.StatusCode != http.StatusServiceUnavailable || !rateLimited.BreakerEligible || !rateLimited.Retryable {
		t.Fatalf("unexpected rate limit classification: %+v", rateLimited)
	}

	serverErr := classifyHTTPError(http.StatusBadGateway, "bad upstream")
	if serverErr.StatusCode != http.StatusBadGateway || !serverErr.BreakerEligible || !serverErr.Retryable {
		t.Fatalf("unexpected server classification: %+v", serverErr)
	}

	clientErr := classifyHTTPError(http.StatusBadRequest, "broken payload")
	if clientErr.BreakerEligible || clientErr.Retryable {
		t.Fatalf("expected non-retryable client error: %+v", clientErr)
	}
}

func TestRouterHealth_ReadyAndUnavailable(t *testing.T) {
	svc := NewRouterService()
	svc.setModelPolicyConfig(policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"m1"}},
		Aliases:        map[string]string{"simple": "simple", "default": "simple"},
	})

	health := svc.Health()
	if !health.Ready || health.Status != "ok" {
		t.Fatalf("expected healthy router, got %+v", health)
	}

	attempt := modelAttempt{Profile: "simple", Model: "m1"}
	svc.recordFailure(attempt, http.StatusBadGateway, "x")
	svc.recordFailure(attempt, http.StatusBadGateway, "x")
	svc.recordFailure(attempt, http.StatusBadGateway, "x")

	health = svc.Health()
	if health.Ready {
		t.Fatalf("expected unhealthy router after circuit open, got %+v", health)
	}
	if health.Status != "unavailable" {
		t.Fatalf("expected unavailable status, got %+v", health)
	}
}
