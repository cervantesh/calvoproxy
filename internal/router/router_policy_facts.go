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
	if policy.RequestedLimits.BodyBytes > 0 {
		add("request_body_class", bodySizeClass(policy.RequestedLimits.BodyBytes))
	}
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

func bodySizeClass(bytes int64) string {
	switch {
	case bytes >= 1<<20:
		return "large"
	case bytes >= 1<<10:
		return "medium"
	default:
		return "small"
	}
}
