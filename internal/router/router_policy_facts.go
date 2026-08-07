package router

import (
	"strconv"
	"strings"

	cervofacts "github.com/cervantesh/cervo-rules/v3/facts"
)

type derivedPolicyFacts struct {
	Facts  []cervofacts.Fact
	Budget cervofacts.EvalOptions
}

func derivePolicyFacts(request requestFacts, policy policyRequestFacts, health ProxyHealth) derivedPolicyFacts {
	out := derivedPolicyFacts{
		Budget: cervofacts.EvalOptions{
			MaxIterations:                 1,
			MaxFacts:                      32,
			MaxBindings:                   32,
			ExpensiveRuleBindingThreshold: 16,
			Trace:                         cervofacts.TraceDisabled,
		},
	}
	add := func(predicate, value string) {
		value = strings.TrimSpace(value)
		if value == "" || len(out.Facts) >= out.Budget.MaxFacts {
			return
		}
		out.Facts = append(out.Facts, fact1(predicate, value))
	}

	add("request_operation", string(request.OperationHint))
	add("request_channel", request.Channel)
	add("request_risk", request.Risk)
	add("request_profile", policy.Metadata["profile"])
	if request.Stream || policy.RequestedLimits.Stream {
		add("request_feature", "stream")
	}
	if policy.RequestedLimits.Tools {
		add("request_feature", "tools")
	}
	if policy.RequestedLimits.Images {
		add("request_feature", "images")
	}
	if policy.RequestedLimits.MaxTokens > 0 {
		add("request_requested_tokens", strconv.Itoa(policy.RequestedLimits.MaxTokens))
	}
	// There is no request_body_class fact any more. It was derived by
	// bodySizeClass from two thresholds hardcoded in Go (1<<20 and 1<<10), and
	// it decided nothing: this slice is consumed only for its length. Two
	// silent copies of "how big is big" is the defect, not the fix. The
	// decision-bearing threshold now lives in policy-rules.yaml as the
	// deny-oversized-body rule, where editing it moves PolicyHash.
	if health.Status != "" {
		add("proxy_status", health.Status)
	}
	if health.Ready {
		add("proxy_readiness", "ready")
	} else if health.Status != "" {
		add("proxy_readiness", "not_ready")
	}
	if health.OpenCircuitCount > 0 {
		add("proxy_circuit_state", "has_open_circuits")
	}
	return out
}

func fact1(predicate, value string) cervofacts.Fact {
	return cervofacts.Fact{
		Predicate: cervofacts.Atom(predicate),
		Terms: []cervofacts.Term{{
			Kind:  cervofacts.TermConst,
			Value: value,
		}},
	}
}
