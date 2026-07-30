package router

import (
	"testing"

	cervofacts "github.com/cervantesh/cervo-rules/v3/facts"
)

func TestDerivedPolicyFactsAreBoundedAndTransportNeutral(t *testing.T) {
	request := requestFacts{
		OperationHint: capChatCompletion,
		Channel:       "telegram",
		Risk:          "sensitive",
		Stream:        true,
	}
	policy := policyRequestFacts{
		Metadata: map[string]string{
			"profile":         "coding",
			"requested_model": "model-a",
		},
		RequestedLimits: RequestedLimits{BodyBytes: 2048, MaxTokens: 128, Tools: true, Images: true},
	}
	health := ProxyHealth{Ready: false, Status: "degraded", OpenCircuitCount: 2}

	derived := derivePolicyFacts(request, policy, health)

	if len(derived.Facts) > derived.Budget.MaxFacts {
		t.Fatalf("derived facts exceeded budget: facts=%d budget=%d", len(derived.Facts), derived.Budget.MaxFacts)
	}
	for _, want := range []cervofacts.Fact{
		fact1("request_operation", "chat_completion"),
		fact1("request_channel", "telegram"),
		fact1("request_risk", "sensitive"),
		fact1("request_profile", "coding"),
		fact1("request_feature", "stream"),
		fact1("request_feature", "tools"),
		fact1("request_feature", "images"),
		fact1("proxy_readiness", "not_ready"),
		fact1("proxy_status", "degraded"),
		fact1("proxy_circuit_state", "has_open_circuits"),
	} {
		if !hasFact(derived.Facts, want) {
			t.Fatalf("expected derived fact %+v in %+v", want, derived.Facts)
		}
	}
	if derived.Budget.Trace != cervofacts.TraceDisabled {
		t.Fatalf("hot-path policy facts must disable trace by default: %+v", derived.Budget)
	}
}

func hasFact(facts []cervofacts.Fact, want cervofacts.Fact) bool {
	for _, fact := range facts {
		if fact.Predicate != want.Predicate || len(fact.Terms) != len(want.Terms) {
			continue
		}
		matched := true
		for i := range fact.Terms {
			if fact.Terms[i] != want.Terms[i] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}
