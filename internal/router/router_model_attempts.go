package router

import (
	cervomodelpolicy "github.com/cervantesh/cervo-model-policy"
	cervorules "github.com/cervantesh/cervo-rules/v3/core"
)

type PolicyModelAttemptPlanner struct {
	Policy policyConfig
	model  *cervomodelpolicy.Policy
}

func NewPolicyModelAttemptPlanner(policy policyConfig) PolicyModelAttemptPlanner {
	normalized := cervomodelpolicy.NormalizeConfig(policy)
	return PolicyModelAttemptPlanner{Policy: normalized, model: cervomodelpolicy.NewPolicy(normalized)}
}

func (p PolicyModelAttemptPlanner) Plan(decision policyDecision, category string, requestedModel string) []modelAttempt {
	modelPolicy := p.model
	if modelPolicy == nil {
		modelPolicy = cervomodelpolicy.NewPolicy(p.Policy)
	}
	modelDecision := modelPolicy.Select(cervomodelpolicy.Request{
		Profile:        category,
		RequestedModel: requestedModel,
	})
	models := modelDecision.ModelChain

	providers := []cervorules.Executor{decision.Executor}
	providers = append(providers, decision.FallbackExecutors...)
	if len(providers) == 0 || providers[0] == "" {
		providers = []cervorules.Executor{providerOpenRouter}
	}

	attempts := make([]modelAttempt, 0, len(models)*len(providers))
	seen := map[string]struct{}{}
	for _, model := range models {
		for _, provider := range providers {
			key := string(provider) + "\x00" + model
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			attempts = append(attempts, modelAttempt{
				Profile:       modelDecision.Profile,
				Model:         model,
				Provider:      provider,
				BreakerPolicy: decision.BreakerPolicy,
			})
		}
	}
	return attempts
}
