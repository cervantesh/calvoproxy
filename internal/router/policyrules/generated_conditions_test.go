package policyrules

import (
	"context"
	"errors"
	"testing"

	cervorules "github.com/cervantesh/cervo-rules/v3/core"
)

// These assemble generatedEngine directly instead of going through Build.
//
// That is deliberate and it is the only way to reach this code. The generator
// reads policy-rules.yaml, which declares no deny rules and no `requires:`
// clauses, so it emits `denies := []generatedDeny{}` and `requires: nil` on
// every route as literals. No runtime config can change either — Build's knobs
// are targets, executors, fallbacks, trusted users and the condition evaluator,
// and none of them adds a condition to a route. So the deny loop and everything
// in conditionsHold past its empty-slice fast path never execute in production.
//
// They are still worth pinning, because what they implement is a fail-closed
// rule: a condition that cannot be answered must DENY, not be read as a
// non-match. The day policy-rules.yaml grows its first `requires:` clause, that
// rule starts governing real traffic. A guard whose failure mode nobody has
// exercised is a guard nobody knows the shape of — which is the same defect,
// one layer down, as the trusted-user gate that was satisfied by an
// unauthenticated header.
//
// If these break after a regeneration, the generator changed how it emits
// guards. That is worth knowing before it reaches traffic, not after.

const testCondition = cervorules.Condition("body_under_limit")

// engineWithCondition returns an engine whose single route is gated on one
// condition, as the generator would emit for a `requires:` clause.
func engineWithCondition(evaluator cervorules.Conditions) generatedEngine {
	return generatedEngine{
		routes: map[cervorules.Operation]generatedRoute{
			cervorules.Operation("chat_completion"): {
				target:   cervorules.Target("cervocore"),
				id:       "chat_completion",
				reason:   "route matched",
				requires: []cervorules.Condition{testCondition},
			},
		},
		denies:            []generatedDeny{},
		disabledDenies:    map[cervorules.Operation]string{},
		trustedUsers:      map[string]struct{}{},
		defaultExecutor:   cervorules.Executor("openrouter"),
		executorFallbacks: map[cervorules.Executor][]cervorules.Executor{},
		conditions:        evaluator,
	}
}

func chatRequest() cervorules.Request {
	return cervorules.Request{Operation: cervorules.Operation("chat_completion")}
}

// The load-bearing case. A policy that requires a condition, with no evaluator
// wired up, must deny — and must say why, rather than returning a bare false
// that reads like an ordinary non-match.
func TestGuardedRouteDeniesWhenNoEvaluatorIsConfigured(t *testing.T) {
	engine := engineWithCondition(nil)

	result, err := engine.Decide(context.Background(), chatRequest())
	if err == nil {
		t.Fatalf("a policy requiring conditions with no evaluator must fail loudly, got %+v", result.Decision)
	}
	if result.Decision.Allow {
		t.Fatalf("the decision must not allow: %+v", result.Decision)
	}

	var policyErr cervorules.Error
	if !errors.As(err, &policyErr) {
		t.Fatalf("error is not a cervorules.Error: %T %v", err, err)
	}
	if policyErr.Code != cervorules.ErrorCodeMissingConditions {
		t.Fatalf("code = %q, want %q", policyErr.Code, cervorules.ErrorCodeMissingConditions)
	}
	if policyErr.Severity != cervorules.SeverityFatal {
		t.Fatalf("severity = %q, want %q — a missing evaluator is not recoverable", policyErr.Severity, cervorules.SeverityFatal)
	}
}

// An evaluator that errors must propagate, not be swallowed into a non-match.
// The distinction matters: "the condition does not hold" is a policy outcome,
// "I could not tell" is a fault, and only one of them should be reported as a
// decision.
func TestGuardedRouteSurfacesAnEvaluatorFailure(t *testing.T) {
	boom := errors.New("condition backend unavailable")
	engine := engineWithCondition(cervorules.ConditionFunc(
		func(context.Context, cervorules.Condition, cervorules.Request) (bool, error) {
			return false, boom
		}))

	result, err := engine.Decide(context.Background(), chatRequest())
	if !errors.Is(err, boom) {
		t.Fatalf("evaluator failure was not propagated: err=%v decision=%+v", err, result.Decision)
	}
	if result.Decision.Allow {
		t.Fatalf("a failed evaluation must never allow: %+v", result.Decision)
	}
}

// A condition that answers "no" is an ordinary denial, not an error, and the
// trace has to name the route that failed rather than leaving the caller to
// guess which guard closed.
func TestGuardedRouteDeniesWhenTheConditionDoesNotHold(t *testing.T) {
	var asked []cervorules.Condition
	engine := engineWithCondition(cervorules.ConditionFunc(
		func(_ context.Context, condition cervorules.Condition, _ cervorules.Request) (bool, error) {
			asked = append(asked, condition)
			return false, nil
		}))

	result, err := engine.DecideWithOptions(context.Background(), chatRequest(),
		cervorules.NewDecisionOptions(cervorules.WithTrace(true)))
	if err != nil {
		t.Fatalf("an unmet condition is a decision, not a fault: %v", err)
	}
	if result.Decision.Allow {
		t.Fatalf("route allowed with its condition unmet: %+v", result.Decision)
	}
	if len(asked) != 1 || asked[0] != testCondition {
		t.Fatalf("evaluator was asked %v, want exactly [%s]", asked, testCondition)
	}
	if result.Trace == nil || len(result.Trace.Steps) == 0 {
		t.Fatal("a condition-driven denial must still be traceable")
	}
	step := result.Trace.Steps[len(result.Trace.Steps)-1]
	if step.Matched {
		t.Fatalf("the step for a route whose condition failed reports Matched=true: %+v", step)
	}
	if step.Name != "chat_completion" {
		t.Fatalf("step names %q, want the route id", step.Name)
	}
}

func TestGuardedRouteAllowsWhenEveryConditionHolds(t *testing.T) {
	engine := engineWithCondition(cervorules.ConditionFunc(
		func(context.Context, cervorules.Condition, cervorules.Request) (bool, error) {
			return true, nil
		}))

	result, err := engine.Decide(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !result.Decision.Allow {
		t.Fatalf("route denied with its condition met: %+v", result.Decision)
	}
	if result.Decision.Target != cervorules.Target("cervocore") {
		t.Fatalf("target = %q, want cervocore", result.Decision.Target)
	}
}

// conditionsHold must stop at the first condition that does not hold. Asking
// the rest is wasted work at best, and at worst it hides that the evaluator has
// side effects the interface says it must not have.
func TestConditionsAreNotEvaluatedPastTheFirstFailure(t *testing.T) {
	var asked []cervorules.Condition
	engine := engineWithCondition(cervorules.ConditionFunc(
		func(_ context.Context, condition cervorules.Condition, _ cervorules.Request) (bool, error) {
			asked = append(asked, condition)
			return false, nil
		}))
	route := engine.routes[cervorules.Operation("chat_completion")]
	route.requires = []cervorules.Condition{testCondition, cervorules.Condition("never_reached")}
	engine.routes[cervorules.Operation("chat_completion")] = route

	if _, err := engine.Decide(context.Background(), chatRequest()); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if len(asked) != 1 {
		t.Fatalf("evaluator asked %v, want it to stop at the first failure", asked)
	}
}

// The deny loop. Deny rules are ordered and evaluated before routes, so a deny
// beats an otherwise-valid route — and an empty operation on the deny applies
// it to everything.
func TestDenyRulesAreEvaluatedBeforeRoutes(t *testing.T) {
	engine := engineWithCondition(cervorules.ConditionFunc(
		func(context.Context, cervorules.Condition, cervorules.Request) (bool, error) {
			return true, nil
		}))
	engine.denies = []generatedDeny{{
		operation: cervorules.Operation("chat_completion"),
		id:        "chat_completion.blocked",
		reason:    "blocked by policy",
	}}

	result, err := engine.DecideWithOptions(context.Background(), chatRequest(),
		cervorules.NewDecisionOptions(cervorules.WithTrace(true)))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if result.Decision.Allow {
		t.Fatalf("a matching deny rule did not beat the route: %+v", result.Decision)
	}
	if result.Decision.Reason != "blocked by policy" {
		t.Fatalf("reason = %q, want the deny rule's reason", result.Decision.Reason)
	}
	if result.Trace == nil || len(result.Trace.Steps) != 1 {
		t.Fatalf("expected exactly the deny step in the trace: %+v", result.Trace)
	}
	if step := result.Trace.Steps[0]; step.Name != "chat_completion.blocked" || !step.Matched {
		t.Fatalf("unexpected deny step: %+v", step)
	}
}

func TestDenyRuleForAnotherOperationIsSkipped(t *testing.T) {
	engine := engineWithCondition(cervorules.ConditionFunc(
		func(context.Context, cervorules.Condition, cervorules.Request) (bool, error) {
			return true, nil
		}))
	engine.denies = []generatedDeny{{
		operation: cervorules.Operation("embedding"),
		id:        "embedding.blocked",
		reason:    "blocked by policy",
	}}

	result, err := engine.DecideWithOptions(context.Background(), chatRequest(),
		cervorules.NewDecisionOptions(cervorules.WithTrace(true)))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !result.Decision.Allow {
		t.Fatalf("a deny rule for another operation blocked this one: %+v", result.Decision)
	}
	// Skipped by the operation filter means it never became a step: a trace
	// listing rules that were never evaluated is a trace that misleads.
	for _, step := range result.Trace.Steps {
		if step.Name == "embedding.blocked" {
			t.Fatalf("a deny rule that did not apply appears in the trace: %+v", result.Trace.Steps)
		}
	}
}

// An unanswerable deny guard must fail the whole decision rather than being
// read as "this deny does not apply" — the same fail-closed rule as on routes,
// and the direction of the mistake is what matters: it must not fall through to
// an allow.
func TestDenyRuleWithAnUnanswerableGuardFailsTheDecision(t *testing.T) {
	engine := engineWithCondition(nil)
	engine.denies = []generatedDeny{{
		operation: cervorules.Operation("chat_completion"),
		id:        "chat_completion.conditional_block",
		reason:    "blocked by policy",
		requires:  []cervorules.Condition{testCondition},
	}}

	result, err := engine.Decide(context.Background(), chatRequest())
	if err == nil {
		t.Fatalf("an unanswerable deny guard produced a decision: %+v", result.Decision)
	}
	if result.Decision.Allow {
		t.Fatalf("it must not fall through to an allow: %+v", result.Decision)
	}
}

// mergeRuntimeConfig must carry the caller's evaluator through. The comment on
// that branch records a real failure: dropping it made every condition-gated
// policy fail to build, because ValidateConfig then saw no evaluator at all.
// This asserts the fix rather than the comment.
func TestBuildCarriesTheCallersConditionEvaluator(t *testing.T) {
	factory := NewPolicyFactory()
	config := factory.DefaultConfig()
	config.Conditions = cervorules.ConditionFunc(
		func(context.Context, cervorules.Condition, cervorules.Request) (bool, error) {
			return true, nil
		})

	built, err := factory.Build(context.Background(), config)
	if err != nil {
		t.Fatalf("build with an evaluator: %v", err)
	}
	engine, ok := built.(generatedEngine)
	if !ok {
		t.Fatalf("Build returned %T, not the generated engine", built)
	}
	if engine.conditions == nil {
		t.Fatal("the evaluator was dropped between the config and the engine")
	}
}

func TestBuildHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	factory := NewPolicyFactory()
	if _, err := factory.Build(ctx, factory.DefaultConfig()); err == nil {
		t.Fatal("a cancelled context must not build an engine")
	}
}

// appendRuntimeValidationError stamps the offending field onto whatever the
// vocabulary returned. Its three arms take three different error shapes, and
// the field is what tells an operator WHICH setting is wrong — a validation
// error naming no field is a validation error nobody can act on.
func TestValidationErrorsAreStampedWithTheirField(t *testing.T) {
	if errs := appendRuntimeValidationError(nil, nil, "ignored"); len(errs) != 0 {
		t.Fatalf("a nil error produced %d entries", len(errs))
	}

	single := appendRuntimeValidationError(nil, cervorules.Error{Code: "bad", Reason: "single"}, "default_executor")
	if len(single) != 1 || single[0].Field != "default_executor" {
		t.Fatalf("single error not stamped: %+v", single)
	}

	many := appendRuntimeValidationError(nil, cervorules.Errors{
		{Code: "bad", Reason: "first"},
		{Code: "bad", Reason: "second"},
	}, "executor_fallbacks[0]")
	if len(many) != 2 {
		t.Fatalf("expected both errors, got %+v", many)
	}
	for _, err := range many {
		if err.Field != "executor_fallbacks[0]" {
			t.Fatalf("error not stamped: %+v", err)
		}
	}

	// Anything else has to be wrapped rather than dropped: an error the
	// vocabulary did not shape is still a reason the config is invalid.
	plain := appendRuntimeValidationError(nil, errors.New("something else"), "trusted_users")
	if len(plain) != 1 {
		t.Fatalf("a plain error was dropped: %+v", plain)
	}
	if plain[0].Code != cervorules.ErrorCodeInvalidRuntimeConfig || plain[0].Field != "trusted_users" {
		t.Fatalf("plain error not wrapped as an invalid-config error: %+v", plain[0])
	}
	if plain[0].Reason != "something else" {
		t.Fatalf("the original reason was lost: %+v", plain[0])
	}
}
