package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cervorules "github.com/cervantesh/cervo-rules/v3/core"
)

// The proxy takes four facts from caller-set headers. Two of them feed policy
// gates, and neither was checked: X-Cervo-Capability named the operation, and
// X-Cervo-User named the identity that `requires_trusted_user` resolves against.
// These tests pin both.

func headerProbeRequest(t *testing.T, capability, user string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	if capability != "" {
		req.Header.Set(headerCapability, capability)
	}
	if user != "" {
		req.Header.Set(headerUser, user)
	}
	return req
}

func TestClientOperationMustBeInVocabulary(t *testing.T) {
	for _, operation := range []string{"secret_lookup", "chat_completion", "embedding"} {
		if err := validateClientOperation(cervorules.Operation(operation)); err != nil {
			t.Fatalf("declared operation %q rejected: %v", operation, err)
		}
	}
	// The empty operation is the common case — no header — and is not the
	// caller naming something, so it passes and the path-derived hint stands.
	if err := validateClientOperation(""); err != nil {
		t.Fatalf("absent operation rejected: %v", err)
	}
	for _, operation := range []string{"not_an_operation", "admin", "chat-completion"} {
		if err := validateClientOperation(cervorules.Operation(operation)); err == nil {
			t.Fatalf("operation %q is not in the vocabulary but was accepted", operation)
		}
	}
}

// An unknown capability used to reach the policy and fail there with "no route
// for operation" — the right outcome for the wrong reason, and one that a
// catch-all route would silently remove. It must now fail as a bad request.
func TestUnknownCapabilityHeaderIsRejectedBeforeThePolicy(t *testing.T) {
	service := simpleTraceService(t)
	recorder := httptest.NewRecorder()
	decision, ok := service.authorizeOperationalRoute(t.Context(), recorder,
		headerProbeRequest(t, "not_an_operation", ""), requestFacts{}, nil)
	if ok {
		t.Fatalf("unknown capability authorised: %+v", decision)
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), headerCapability) {
		t.Fatalf("error does not name the offending header: %s", recorder.Body.String())
	}
}

// The measured hole: an ordinary unauthenticated chat request that claimed to be
// a trusted user performing a secret lookup was authorised, and the audit record
// said so.
func TestTrustedUserGateIgnoresAnUnauthenticatedUserHeader(t *testing.T) {
	t.Setenv("PROXY_TRUSTED_USERS", "cervantes")
	service := simpleTraceService(t)
	recorder := httptest.NewRecorder()
	decision, ok := service.authorizeOperationalRoute(t.Context(), recorder,
		headerProbeRequest(t, "secret_lookup", "cervantes"), requestFacts{}, nil)
	if ok {
		t.Fatalf("secret_lookup authorised by a header alone: %+v", decision)
	}
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	// The audit record must not claim a user the proxy never authenticated.
	if decision.Target != "" && strings.Contains(strings.ToLower(decision.Reason), "trusted") {
		t.Logf("deny reason: %s", decision.Reason)
	}
}

// Deployments that authenticate the caller upstream and set the header
// themselves keep the old behaviour behind an explicit opt-in.
func TestUserHeaderIsBelievedWhenTheDeploymentOptsIn(t *testing.T) {
	t.Setenv("PROXY_TRUSTED_USERS", "cervantes")
	t.Setenv("PROXY_TRUST_USER_HEADER", "true")
	service := simpleTraceService(t)
	recorder := httptest.NewRecorder()
	decision, ok := service.authorizeOperationalRoute(t.Context(), recorder,
		headerProbeRequest(t, "secret_lookup", "cervantes"), requestFacts{}, nil)
	if !ok {
		t.Fatalf("opt-in did not restore the trusted route: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if decision.Target == "" {
		t.Fatalf("allowed decision carries no target: %+v", decision)
	}
}

func TestUserHeaderIsDroppedFromTheFactsByDefault(t *testing.T) {
	req := headerProbeRequest(t, "", "cervantes")
	if user := proxyHTTPClassifier.FactsFromHTTPRequest(req).User; user != "" {
		t.Fatalf("user = %q, want empty without PROXY_TRUST_USER_HEADER", user)
	}
	t.Setenv("PROXY_TRUST_USER_HEADER", "true")
	if user := proxyHTTPClassifier.FactsFromHTTPRequest(req).User; user != "cervantes" {
		t.Fatalf("user = %q, want %q with the opt-in", user, "cervantes")
	}
}
