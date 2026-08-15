package router

import (
	"context"
	"net/http/httptest"
	"testing"
)

func reasoningTestIndex() *capabilityIndex {
	idx := newCapabilityIndex(map[string][]string{
		"effort-model":    {capReasoning, capReasoningEffort},
		"object-model":    {capReasoning},
		"incapable-model": {capTools},
	})
	return idx
}

func newReasoningService(profiles reasoningProfiles) *RouterService {
	return &RouterService{reasoningProfiles: profiles, capabilities: reasoningTestIndex()}
}

func TestApplyReasoningEffortUsesShapeTheModelAdvertises(t *testing.T) {
	idx := reasoningTestIndex()

	flat := map[string]interface{}{}
	applyReasoningEffort(flat, reasoningEffortHigh, "effort-model", idx)
	if flat["reasoning_effort"] != "high" {
		t.Fatalf("a model advertising reasoning_effort should get the flat field, got %#v", flat)
	}
	if _, ok := flat["reasoning"]; ok {
		t.Fatalf("the flat field and the object must not both be sent: %#v", flat)
	}

	object := map[string]interface{}{}
	applyReasoningEffort(object, reasoningEffortLow, "object-model", idx)
	reasoning, ok := object["reasoning"].(map[string]interface{})
	if !ok || reasoning["effort"] != "low" {
		t.Fatalf("a model advertising only reasoning should get the object form, got %#v", object)
	}
}

// The whole safety argument for enabling this by profile is that a model which
// cannot take the parameter never receives it — otherwise turning on a profile
// default would convert working free models into 400s.
func TestApplyReasoningEffortSkipsModelsWithoutTheCapability(t *testing.T) {
	for _, model := range []string{"incapable-model", "model-with-no-capability-data"} {
		body := map[string]interface{}{}
		applyReasoningEffort(body, reasoningEffortHigh, model, reasoningTestIndex())
		if len(body) != 0 {
			t.Fatalf("%s must receive no reasoning parameter, got %#v", model, body)
		}
	}
}

func TestResolveReasoningEffortPrecedence(t *testing.T) {
	svc := newReasoningService(reasoningProfiles{"reasoning": reasoningEffortHigh})

	t.Setenv("PROXY_REASONING_EFFORT", "low")

	// Profile beats the global env floor.
	if effort, ok := svc.resolveReasoningEffort(context.Background(), "reasoning"); !ok || effort != reasoningEffortHigh {
		t.Fatalf("profile default should win over env, got %q ok=%v", effort, ok)
	}
	// A profile with no entry falls through to the env floor.
	if effort, ok := svc.resolveReasoningEffort(context.Background(), "bulk"); !ok || effort != reasoningEffortLow {
		t.Fatalf("unlisted profile should fall back to env, got %q ok=%v", effort, ok)
	}
	// The per-request override beats the profile.
	ctx := WithReasoningEffort(context.Background(), reasoningEffortMedium)
	if effort, ok := svc.resolveReasoningEffort(ctx, "reasoning"); !ok || effort != reasoningEffortMedium {
		t.Fatalf("request override should win over profile, got %q ok=%v", effort, ok)
	}
}

func TestResolveReasoningEffortSilentWithoutConfiguration(t *testing.T) {
	t.Setenv("PROXY_REASONING_EFFORT", "")
	svc := newReasoningService(nil)
	if effort, ok := svc.resolveReasoningEffort(context.Background(), "coding"); ok {
		t.Fatalf("an unconfigured proxy must send no effort at all, got %q", effort)
	}
}

// An explicit caller opinion is authoritative: the operator default exists to
// decide for clients that did not decide, not to overrule ones that did.
func TestRequestBodyForAttemptNeverOverridesTheCaller(t *testing.T) {
	svc := newReasoningService(reasoningProfiles{"reasoning": reasoningEffortHigh})

	body := map[string]interface{}{"reasoning_effort": "low"}
	out := svc.requestBodyForAttempt(context.Background(), body, modelAttempt{Profile: "reasoning", Model: "effort-model"})
	if out["reasoning_effort"] != "low" {
		t.Fatalf("caller's explicit effort must survive, got %#v", out["reasoning_effort"])
	}

	// A caller using the object form is equally explicit, including a shape this
	// proxy would not write itself.
	budget := map[string]interface{}{"reasoning": map[string]interface{}{"max_tokens": 2048}}
	out = svc.requestBodyForAttempt(context.Background(), budget, modelAttempt{Profile: "reasoning", Model: "effort-model"})
	if _, injected := out["reasoning_effort"]; injected {
		t.Fatalf("a caller-supplied reasoning object must not be supplemented, got %#v", out)
	}
}

func TestRequestBodyForAttemptInjectsProfileDefault(t *testing.T) {
	svc := newReasoningService(reasoningProfiles{"reasoning": reasoningEffortHigh})
	body := map[string]interface{}{"messages": []interface{}{}}

	out := svc.requestBodyForAttempt(context.Background(), body, modelAttempt{Profile: "reasoning", Model: "effort-model"})
	if out["reasoning_effort"] != "high" {
		t.Fatalf("profile default should be injected, got %#v", out)
	}
	// The shared request body must not be mutated: later fallbacks reuse it and
	// may target a model with different reasoning support.
	if _, leaked := body["reasoning_effort"]; leaked {
		t.Fatalf("the caller's body was mutated: %#v", body)
	}
}

func TestWithRequestReasoningEffortReadsHeaderAndQuery(t *testing.T) {
	header := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	header.Header.Set(headerReasoning, "HIGH") // case-insensitive on purpose
	if effort, ok := requestReasoningEffort(withRequestReasoningEffort(context.Background(), header)); !ok || effort != reasoningEffortHigh {
		t.Fatalf("header override not applied, got %q ok=%v", effort, ok)
	}

	query := httptest.NewRequest("POST", "/v1/chat/completions?reasoning=medium", nil)
	if effort, ok := requestReasoningEffort(withRequestReasoningEffort(context.Background(), query)); !ok || effort != reasoningEffortMedium {
		t.Fatalf("query override not applied, got %q ok=%v", effort, ok)
	}

	// Garbage is ignored rather than forwarded as an invalid upstream value.
	bad := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	bad.Header.Set(headerReasoning, "extreme")
	if _, ok := requestReasoningEffort(withRequestReasoningEffort(context.Background(), bad)); ok {
		t.Fatal("an unrecognised effort must be ignored, not propagated")
	}
}

func TestParseReasoningProfilesRejectsInvalidEfforts(t *testing.T) {
	profiles, ok := parseReasoningProfiles([]byte(`{"Reasoning":{"reasoning":"high","bulk":"turbo","":"low"}}`))
	if !ok {
		t.Fatal("expected the Reasoning section to parse")
	}
	if len(profiles) != 1 || profiles["reasoning"] != reasoningEffortHigh {
		t.Fatalf("only the valid entry should survive, got %#v", profiles)
	}
}

// The shipped policy must stay loadable by the code that reads it, or the
// profile defaults silently do nothing in production.
func TestShippedPolicyReasoningSectionLoads(t *testing.T) {
	profiles, ok := parseReasoningProfiles(defaultModelConfigJSON)
	if !ok || len(profiles) == 0 {
		t.Fatalf("embedded default policy should define a Reasoning section, got %#v ok=%v", profiles, ok)
	}
	if profiles["reasoning"] != reasoningEffortHigh {
		t.Fatalf("the reasoning profile should ship at high effort, got %q", profiles["reasoning"])
	}
}
