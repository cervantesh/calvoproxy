package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cervantesh/calvoproxy/internal/router/policyvocab"
)

// The body-size threshold moved out of Go and into policy-rules.yaml. These
// pin what that bought: the number now decides something, and it decides it
// from the file PolicyHash covers.

const policyBodyLimit = 10 << 20 // must equal deny-oversized-body in policy-rules.yaml

func bodyOfSize(n int) []byte {
	body := make([]byte, n)
	for i := range body {
		body[i] = 'x'
	}
	return body
}

func decideForBody(t *testing.T, body []byte) (policyDecision, bool, *httptest.ResponseRecorder) {
	t.Helper()
	service := simpleTraceService(t)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	decision, ok := service.authorizeOperationalRoute(t.Context(), recorder, request,
		requestFacts{OperationHint: capChatCompletion}, body)
	return decision, ok, recorder
}

func TestPolicyDeniesABodyOverItsDeclaredThreshold(t *testing.T) {
	decision, ok, recorder := decideForBody(t, bodyOfSize(policyBodyLimit+1))
	if ok {
		t.Fatalf("a body one byte over the policy threshold was allowed: %+v", decision)
	}
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	if !strings.Contains(decision.Reason, "exceeds the size this policy allows") {
		t.Fatalf("denial does not carry the policy's reason: %q", decision.Reason)
	}
}

func TestPolicyAllowsABodyAtItsDeclaredThreshold(t *testing.T) {
	decision, ok, recorder := decideForBody(t, bodyOfSize(policyBodyLimit))
	if !ok {
		t.Fatalf("a body exactly at the threshold was denied: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if decision.Target == "" {
		t.Fatalf("allowed decision carries no target: %+v", decision)
	}
}

// An absent fact must not fail the decision. The policy declares `default: 0`
// precisely so a request with no body is decided rather than rejected — without
// it every bodyless request would fault, because the generated frame treats an
// absent fact as a fault and not as an unsatisfied guard.
func TestPolicyDecidesARequestWithNoBody(t *testing.T) {
	if _, ok, recorder := decideForBody(t, nil); !ok {
		t.Fatalf("a request with no body was not decided: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// The name the proxy writes and the name the policy reads are the same
// generated constant. If they ever diverge the decision would be made on an
// absent fact, which is exactly the failure the constant exists to prevent.
func TestBodyFactIsWrittenUnderTheNameThePolicyReads(t *testing.T) {
	if policyvocab.FactBodyBytes != "body_bytes" {
		t.Fatalf("FactBodyBytes = %q; policy-rules.yaml declares body_bytes", policyvocab.FactBodyBytes)
	}
}

// The compiled matcher reports which leaf decided, and that text now carries
// the threshold itself. It is admin-only for the reason policyTraceStep already
// documents — it describes the internals of the authorisation rules — so the
// un-gated projection must not carry the number.
func TestThresholdDoesNotReachTheUnGatedTraceChannel(t *testing.T) {
	t.Setenv("PROXY_ROUTE_TRACE", "on")
	service := simpleTraceService(t)
	recorder := httptest.NewRecorder()
	body := bodyOfSize(policyBodyLimit + 1)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	decision, _ := service.authorizeOperationalRoute(t.Context(), recorder, request,
		requestFacts{OperationHint: capChatCompletion}, body)

	trace := &routeTrace{RuleID: decision.RuleID, PolicySteps: decision.PolicySteps}
	ungated, err := json.Marshal(trace.view(false))
	if err != nil {
		t.Fatalf("un-gated view does not marshal: %v", err)
	}
	if strings.Contains(string(ungated), "10485760") {
		t.Fatalf("the policy threshold reached the un-gated channel: %s", ungated)
	}

	gated, err := json.Marshal(trace.view(true))
	if err != nil {
		t.Fatalf("admin view does not marshal: %v", err)
	}
	if !strings.Contains(string(gated), "body_bytes") {
		t.Fatalf("the admin channel lost the reason the rule fired: %s", gated)
	}
}
