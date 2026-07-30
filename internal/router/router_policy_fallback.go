package router

import (
	"context"
	"errors"

	cervorules "github.com/cervantesh/cervo-rules/v3/core"
)

type denyAllPolicyEngine string

func (e denyAllPolicyEngine) Decide(_ context.Context, req cervorules.Request) (cervorules.DecisionResult, error) {
	reason := string(e)
	if reason == "" {
		reason = "policy unavailable"
	}
	decision := cervorules.Decision{
		Allow:  false,
		Reason: reason,
	}
	result := cervorules.NewDecisionResult(req, decision,
		cervorules.WithTrace(true),
		cervorules.WithObservation(true),
	)
	return result, errors.New(reason)
}

func (e denyAllPolicyEngine) DecideWithOptions(_ context.Context, req cervorules.Request, options cervorules.DecisionOptions) (cervorules.DecisionResult, error) {
	reason := string(e)
	if reason == "" {
		reason = "policy unavailable"
	}
	decision := cervorules.Decision{
		Allow:  false,
		Reason: reason,
	}
	result := cervorules.NewDecisionResult(req, decision,
		cervorules.WithTrace(options.TraceEnabled()),
		cervorules.WithObservation(options.ObservationEnabled()),
	)
	return result, errors.New(reason)
}
