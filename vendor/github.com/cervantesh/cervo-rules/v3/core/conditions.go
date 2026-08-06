package core

import (
	"context"
	"sort"
)

// Condition identifies a caller-owned named condition that a policy consults
// before allowing or denying an operation.
//
// Conditions are the seam between a caller that proposes an action and a guard
// that can refuse it. A policy references a condition by name; the consumer
// supplies the evaluator at build time and owns what the name means.
type Condition string

// NewCondition normalizes a caller-owned condition name.
func NewCondition(value string) Condition { return Condition(normalize(value)) }

// Conditions resolves named policy conditions at decision time.
//
// Implementations must be side-effect free. A decision runtime may evaluate the
// same condition zero or more times per request, and callers rely on decisions
// staying pure until the caller itself executes the chosen action.
type Conditions interface {
	Holds(ctx context.Context, condition Condition, req Request) (bool, error)
}

// ConditionFunc adapts a plain function to the Conditions interface.
type ConditionFunc func(ctx context.Context, condition Condition, req Request) (bool, error)

// Holds implements Conditions.
func (f ConditionFunc) Holds(ctx context.Context, condition Condition, req Request) (bool, error) {
	if f == nil {
		return false, Error{
			Code:      ErrorCodeMissingConditions,
			Severity:  SeverityFatal,
			Component: "core",
			Field:     "conditions",
			Value:     string(condition),
			Reason:    "no condition evaluator is configured",
		}
	}
	return f(ctx, condition, req)
}

// ConditionSet is a static map-backed Conditions implementation.
//
// An unknown condition fails closed: it returns a structured error instead of
// reporting false, so a policy can never silently allow an operation whose
// guard was never wired up.
type ConditionSet map[Condition]func(ctx context.Context, req Request) (bool, error)

// Holds implements Conditions.
func (s ConditionSet) Holds(ctx context.Context, condition Condition, req Request) (bool, error) {
	evaluate, ok := s[NewCondition(string(condition))]
	if !ok || evaluate == nil {
		return false, Error{
			Code:       ErrorCodeUnknownCondition,
			Severity:   SeverityFatal,
			Component:  "core",
			Field:      "conditions",
			Value:      string(condition),
			Reason:     "condition is not registered",
			Suggestion: "register the condition on the runtime config before building the policy",
		}
	}
	return evaluate(ctx, req)
}

// Names returns the registered condition names in stable order.
func (s ConditionSet) Names() []Condition {
	out := make([]Condition, 0, len(s))
	for condition := range s {
		out = append(out, condition)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
