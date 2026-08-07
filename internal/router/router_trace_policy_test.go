package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func simpleTraceService(t *testing.T) *RouterService {
	t.Helper()
	return newTestService(t, &http.Client{Transport: failingTransport{}}, policyConfig{
		DefaultProfile: "simple",
		Profiles:       map[string][]string{"simple": {"model-a"}},
		Aliases:        map[string]string{"default": "simple", "simple": "simple"},
	})
}

// traceForRequest runs one request and returns the live trace the ring kept, so
// a test can assert on the struct rather than on one channel's projection of it.
func traceForRequest(t *testing.T, svc *RouterService, r *http.Request) *routeTrace {
	t.Helper()
	rec := httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, r, "k", "")
	id := rec.Header().Get("X-Calvoproxy-Decision-Id")
	if id == "" {
		t.Fatal("no decision id: the request produced no trace")
	}
	trace, ok := svc.traceRingRef().get(id)
	if !ok {
		t.Fatalf("decision %s is not in the ring", id)
	}
	return trace
}

// This is the test for finding A: the spec's §2 schema declared RuleID,
// RequestedModel and CapsRequired, and the code wrote none of the first two. A
// spec field that nobody writes is worse than no spec — a reader trusts it and
// builds on a promise that was never kept. Every field §2 declares as written on
// a normal served request is asserted here, so the next divergence fails a test
// instead of surviving a review.
func TestTrace_WritesEveryFieldTheSpecDeclares(t *testing.T) {
	svc := simpleTraceService(t)
	trace := traceForRequest(t, svc, trustedRequest(
		http.MethodPost, "/v1/chat/completions", `{"messages":[],"model":"model-a"}`))

	if trace.Profile == "" {
		t.Error("spec §2 declares Profile; it is empty")
	}
	if trace.RuleID == "" {
		t.Error("spec §2 declares RuleID; it is empty — the exact divergence finding A named")
	}
	if trace.RequestedModel != "model-a" {
		t.Errorf("spec §2 declares RequestedModel; got %q, want %q", trace.RequestedModel, "model-a")
	}
	if trace.Planned == 0 {
		t.Error("spec §2 declares Planned; it is zero")
	}
	if len(trace.PolicySteps) == 0 {
		t.Error("spec §2 declares PolicySteps; the engine's evaluation trace never arrived")
	}
}

// The rule id is the one thing the policy produced that belongs on the short
// header (spec §6): a closed identifier a consumer resolves against the policy
// it already holds. The free text does not — it would spend the 512-byte budget
// on prose.
func TestTrace_HeaderCarriesRuleIDAndNoPolicyProse(t *testing.T) {
	svc := simpleTraceService(t)
	rec := httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, trustedRequest(
		http.MethodPost, "/v1/chat/completions", `{"messages":[]}`), "k", "")

	route := rec.Header().Get("X-Calvoproxy-Route")
	if !strings.Contains(route, "rid=route.chat_completion") {
		t.Errorf("route header must carry the rule id, got %q", route)
	}
	// Whatever the policy's own reason text says, it is not in the header.
	trace, _ := svc.traceRingRef().get(rec.Header().Get("X-Calvoproxy-Decision-Id"))
	if trace != nil && trace.PolicyReason != "" && strings.Contains(route, trace.PolicyReason) {
		t.Errorf("policy free text leaked into the short header: %q", route)
	}
}

// rid= is omitted rather than truncated when it will not fit. The operation can
// come from the client's X-Cervo-Capability header, so the rule id's length is
// not ours to assume — and a truncated identifier is a wrong identifier that a
// consumer would compare against its policy and silently mismatch.
func TestTrace_HeaderOmitsAnOversizedRuleID(t *testing.T) {
	trace := &routeTrace{ID: "abc", Profile: "simple", RuleID: "route." + strings.Repeat("x", 200)}
	if got := trace.header(); strings.Contains(got, "rid=") {
		t.Errorf("an unrenderable rule id must be omitted, not truncated: %q", got)
	}
	trace.RuleID = "route.chat_completion"
	if got := trace.header(); !strings.Contains(got, "rid=route.chat_completion") {
		t.Errorf("a rule id within the bound must be carried: %q", got)
	}
	// Separator injection through the rule id must not be able to forge a field.
	trace.RuleID = "route.x;p=INJECTED"
	if got := trace.header(); strings.Contains(got, "p=INJECTED") {
		t.Errorf("rule id must be sanitised before it reaches the header: %q", got)
	}
}

// Spec §6, applied to what the policy produced: closed identifiers travel on
// both JSON channels, free-form text only behind the admin gate. The rule id and
// the step NAMES are structural; the policy reason and a step's detail narrate
// the authorisation logic to a caller that passed no gate.
func TestTrace_PolicyChannelSplit(t *testing.T) {
	svc := simpleTraceService(t)
	req := trustedRequest(http.MethodPost, "/v1/chat/completions", `{"messages":[]}`)
	req.Header.Set("X-Calvoproxy-Trace", "full")
	rec := httptest.NewRecorder()
	svc.RouteRequestWithProvider(rec, req, "k", "")

	optIn := rec.Header().Get("X-Calvoproxy-Trace-Json")
	if optIn == "" {
		t.Fatal("opt-in requested but no full trace emitted")
	}
	for _, want := range []string{`"rule_id"`, `"policy_steps"`, `"name"`, `"matched"`} {
		if !strings.Contains(optIn, want) {
			t.Errorf("opt-in JSON is missing the structural field %s: %s", want, optIn)
		}
	}
	for _, forbidden := range []string{`"policy_reason"`, `"detail"`} {
		if strings.Contains(optIn, forbidden) {
			t.Errorf("free-form policy text %s must not cross the un-gated channel: %s", forbidden, optIn)
		}
	}

	id := rec.Header().Get("X-Calvoproxy-Decision-Id")
	found, ok := svc.Decision(id)
	if !ok {
		t.Fatalf("decision %s is not in the ring", id)
	}
	raw, err := json.Marshal(found)
	if err != nil {
		t.Fatalf("decision does not marshal: %v", err)
	}
	var admin traceView
	if err := json.Unmarshal(raw, &admin); err != nil {
		t.Fatalf("admin view does not round-trip: %v", err)
	}
	if admin.RuleID == "" {
		t.Errorf("admin channel lost the rule id: %s", raw)
	}
	if len(admin.PolicySteps) == 0 {
		t.Errorf("admin channel lost the policy steps: %s", raw)
	}
	// The admin view is the only one allowed to carry the free text. Asserted by
	// construction: the same trace rendered for the admin channel differs from
	// the opt-in one exactly in the fields the split names.
	trace, _ := svc.traceRingRef().get(id)
	if trace == nil {
		t.Fatal("trace vanished from the ring")
	}
	trace.PolicyReason = "routed by the coding profile rule"
	trace.PolicySteps[0].Detail = "condition body_bytes under limit held"
	gated, err := json.Marshal(trace.view(true))
	if err != nil {
		t.Fatalf("admin view does not marshal: %v", err)
	}
	ungated, err := json.Marshal(trace.view(false))
	if err != nil {
		t.Fatalf("opt-in view does not marshal: %v", err)
	}
	for _, want := range []string{"routed by the coding profile rule", "condition body_bytes under limit held"} {
		if !strings.Contains(string(gated), want) {
			t.Errorf("admin channel must carry %q: %s", want, gated)
		}
		if strings.Contains(string(ungated), want) {
			t.Errorf("un-gated channel must not carry %q: %s", want, ungated)
		}
	}
}

// Spec §7: PROXY_ROUTE_TRACE=off means nothing is materialised, and that now
// reaches into the policy engine too — it is not asked to explain itself when
// nobody will read the explanation. Asserted on the decision struct rather than
// on the response, because that is where the allocation would be.
func TestTrace_DisabledAsksThePolicyForNoTrace(t *testing.T) {
	decide := func(t *testing.T) policyDecision {
		t.Helper()
		svc := simpleTraceService(t)
		rec := httptest.NewRecorder()
		decision, ok := svc.authorizeOperationalRoute(
			t.Context(), rec, trustedRequest(http.MethodPost, "/v1/chat/completions", `{"messages":[]}`),
			requestFacts{OperationHint: capChatCompletion}, []byte(`{"messages":[]}`))
		if !ok {
			t.Fatalf("policy denied the request: %s", rec.Body.String())
		}
		return decision
	}

	if steps := decide(t).PolicySteps; len(steps) == 0 {
		t.Fatal("with tracing on the decision must carry the engine's steps")
	}

	t.Setenv("PROXY_ROUTE_TRACE", "off")
	if steps := decide(t).PolicySteps; steps != nil {
		t.Errorf("with PROXY_ROUTE_TRACE=off nothing may be materialised, got %#v", steps)
	}
}

// The option builder is pure, so the widening rule is asserted directly: the
// route trace only ever turns the engine's trace ON, and never touches the
// observation, which is sampled on purpose and feeds metrics.
func TestPolicyDecisionOptions_RouteTraceWidensOnlyTheTrace(t *testing.T) {
	req := requestFromFacts(requestFacts{ID: "req-1", OperationHint: capChatCompletion})
	quiet := policyTelemetryConfig{}

	off := policyDecisionOptionsForRequest(req, quiet, false)
	if off.TraceEnabled() || off.ObservationEnabled() {
		t.Errorf("nothing asked for a trace; got trace=%v observation=%v",
			off.TraceEnabled(), off.ObservationEnabled())
	}

	on := policyDecisionOptionsForRequest(req, quiet, true)
	if !on.TraceEnabled() {
		t.Error("the route trace is the engine trace's consumer; it must turn it on")
	}
	if on.ObservationEnabled() {
		t.Error("the route trace must not widen the sampled observation")
	}
}

// Invariant 4 still holds with rid= in the protected set: the worst case a
// hostile caller can construct must not push the header over 512 bytes.
func TestTrace_HeaderCapSurvivesTheRuleID(t *testing.T) {
	trace := &routeTrace{
		ID:           "abcdef0123456789",
		Profile:      strings.Repeat("p", 40),
		RuleID:       "route." + strings.Repeat("o", traceMaxRuleID-6),
		CapsRequired: []string{"vision", "tools"},
		Outcome:      outcomeChainFailed,
		Planned:      12, AfterCaps: 12, Eligible: 12,
		ExcludedByBreaker: 9, ExcludedByQuota: 3,
	}
	for i := 0; i < 24; i++ {
		trace.Attempts = append(trace.Attempts, traceAttempt{
			Model:  "someorg/a-deliberately-long-free-model-slug-number-" + strings.Repeat("z", 20) + ":free",
			Index:  i + 1,
			Status: 429,
		})
	}
	got := trace.header()
	if len(got) > traceMaxHeader {
		t.Fatalf("header is %d bytes, over the %d cap: %q", len(got), traceMaxHeader, got)
	}
	if !strings.Contains(got, "trunc=1") {
		t.Errorf("a trimmed header must say so: %q", got)
	}
	if !strings.Contains(got, "rid=route.") {
		t.Errorf("rid= is protected and must survive trimming: %q", got)
	}
}
