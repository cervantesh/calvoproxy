package policyrules

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cervantesh/calvoproxy/internal/router/policyvocab"
	cervorules "github.com/cervantesh/cervo-rules/v3/core"
	cervoruntime "github.com/cervantesh/cervo-rules/v3/runtime"
)

// The policy grew its first `when:` predicate, so the generated engine now
// parses a typed fact out of the request metadata before any rule runs. That
// frame is fail-closed by construction: a fact that is absent, unparseable or
// outside its declared bounds FAILS the decision rather than reporting the
// predicate as unsatisfied — which would read as "the guard ran and found
// nothing wrong". These pin that, because the difference between those two is
// the difference between a denial and a silent allow.

func chatWith(metadata map[string]string) cervorules.Request {
	return cervorules.Request{Operation: policyvocab.OperationChatCompletion, Metadata: metadata}
}

func TestOversizedBodyIsDeniedByThePolicysOwnThreshold(t *testing.T) {
	result, err := defaultEngine(t).DecideWithOptions(context.Background(),
		chatWith(map[string]string{"body_bytes": "10485761"}),
		cervorules.NewDecisionOptions(cervorules.WithTrace(true)))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if result.Decision.Allow {
		t.Fatalf("a body over the declared threshold was allowed: %+v", result.Decision)
	}
	if !strings.Contains(result.Decision.Reason, "exceeds the size this policy allows") {
		t.Fatalf("reason = %q, want the deny rule's reason", result.Decision.Reason)
	}
	if result.Trace == nil || len(result.Trace.Steps) == 0 {
		t.Fatal("the deny must be traceable")
	}
	step := result.Trace.Steps[0]
	if step.Name != "deny-oversized-body" || !step.Matched {
		t.Fatalf("unexpected step: %+v", step)
	}
	// The step names the leaf that decided, which is what makes a compound
	// predicate debuggable — without it a denial says only that some part of
	// the rule fired.
	if !strings.Contains(step.Reason, "body_bytes") {
		t.Fatalf("step does not name the leaf that decided: %+v", step)
	}
}

func TestBodyAtTheThresholdIsAllowed(t *testing.T) {
	result, err := defaultEngine(t).Decide(context.Background(),
		chatWith(map[string]string{"body_bytes": "10485760"}))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !result.Decision.Allow {
		t.Fatalf("a body exactly at the threshold was denied: %+v", result.Decision)
	}
}

// `gt`, not `gte`: the boundary belongs to the allowed side. Pinning it here
// means a future regeneration cannot flip the comparison unnoticed.
func TestTheThresholdBoundaryBelongsToTheAllowedSide(t *testing.T) {
	for raw, wantAllow := range map[string]bool{
		"10485759": true,
		"10485760": true,
		"10485761": false,
	} {
		result, err := defaultEngine(t).Decide(context.Background(),
			chatWith(map[string]string{"body_bytes": raw}))
		if err != nil {
			t.Fatalf("decide for %s: %v", raw, err)
		}
		if result.Decision.Allow != wantAllow {
			t.Fatalf("body_bytes=%s allowed=%v, want %v", raw, result.Decision.Allow, wantAllow)
		}
	}
}

// The policy declares `default: 0`, so a request that carried no body is
// decided rather than faulted. Without the default every bodyless request
// would fail — which is correct behaviour for an undeclared fact and the wrong
// behaviour for this one.
func TestAnAbsentBodyFactFallsBackToTheDeclaredDefault(t *testing.T) {
	for _, metadata := range []map[string]string{nil, {}, {"body_bytes": "   "}} {
		result, err := defaultEngine(t).Decide(context.Background(), chatWith(metadata))
		if err != nil {
			t.Fatalf("metadata %v was not decided: %v", metadata, err)
		}
		if !result.Decision.Allow {
			t.Fatalf("metadata %v denied: %+v", metadata, result.Decision)
		}
	}
}

func TestAnUnparseableFactFailsTheDecision(t *testing.T) {
	result, err := defaultEngine(t).Decide(context.Background(),
		chatWith(map[string]string{"body_bytes": "not-a-number"}))
	if err == nil {
		t.Fatalf("an unparseable fact produced a decision: %+v", result.Decision)
	}
	if result.Decision.Allow {
		t.Fatalf("it must not fall through to an allow: %+v", result.Decision)
	}
	var policyErr cervorules.Error
	if !errors.As(err, &policyErr) {
		t.Fatalf("error is not a cervorules.Error: %T %v", err, err)
	}
	if policyErr.Code != cervorules.ErrorCodeInvalidFact {
		t.Fatalf("code = %q, want %q", policyErr.Code, cervorules.ErrorCodeInvalidFact)
	}
	if policyErr.Severity != cervorules.SeverityFatal {
		t.Fatalf("severity = %q, want fatal", policyErr.Severity)
	}
}

func TestAFactBelowItsDeclaredMinimumFailsTheDecision(t *testing.T) {
	if _, err := defaultEngine(t).Decide(context.Background(),
		chatWith(map[string]string{"body_bytes": "-1"})); err == nil {
		t.Fatal("a negative body size was accepted; the policy declares min 0")
	}
}

// The observed value is marked sensitive because only the caller knows whether
// a fact carries secrets. It must stay out of the serialized error while
// remaining available in process — an error that leaks the value it is
// complaining about turns a validation failure into a disclosure.
func TestAnInvalidFactValueIsNotSerialized(t *testing.T) {
	_, err := defaultEngine(t).Decide(context.Background(),
		chatWith(map[string]string{"body_bytes": "sk-live-not-a-number"}))
	if err == nil {
		t.Fatal("expected the invalid fact to fail the decision")
	}
	var policyErr cervorules.Error
	if !errors.As(err, &policyErr) {
		t.Fatalf("error is not a cervorules.Error: %T %v", err, err)
	}
	if policyErr.Value != "sk-live-not-a-number" {
		t.Fatalf("the value must stay available in process, got %q", policyErr.Value)
	}
	encoded, marshalErr := json.Marshal(policyErr)
	if marshalErr != nil {
		t.Fatalf("marshal: %v", marshalErr)
	}
	if strings.Contains(string(encoded), "sk-live-not-a-number") {
		t.Fatalf("the observed value was serialized: %s", encoded)
	}
}

// White-box: the policy declares a default and a minimum for its only fact, so
// the no-default and above-maximum arms cannot be reached through it. They are
// the same fail-closed contract as the rest and are reached directly.
func TestFactIntegerValueBoundsContract(t *testing.T) {
	req := cervorules.Request{Metadata: map[string]string{"n": "42"}}

	value, err := factIntegerValue(req, "n", factIntegerBounds{})
	if err != nil || value != 42 {
		t.Fatalf("plain read: value=%d err=%v", value, err)
	}

	if _, err := factIntegerValue(req, "absent", factIntegerBounds{}); err == nil {
		t.Fatal("an absent fact with no declared default must fail the decision")
	} else {
		var policyErr cervorules.Error
		if !errors.As(err, &policyErr) || policyErr.Code != cervorules.ErrorCodeMissingFact {
			t.Fatalf("want %q, got %+v", cervorules.ErrorCodeMissingFact, err)
		}
	}

	if got, err := factIntegerValue(req, "absent", factIntegerBounds{hasDefault: true, def: 7}); err != nil || got != 7 {
		t.Fatalf("declared default not applied: value=%d err=%v", got, err)
	}

	if _, err := factIntegerValue(req, "n", factIntegerBounds{hasMax: true, max: 41}); err == nil {
		t.Fatal("a fact above its declared maximum must fail the decision")
	}
	if _, err := factIntegerValue(req, "n", factIntegerBounds{hasMin: true, min: 43}); err == nil {
		t.Fatal("a fact below its declared minimum must fail the decision")
	}
}

// A metadata key that is present but blank counts as absent, not as an empty
// value to parse. Otherwise every fact would have to defend against "" itself.
func TestFactRawValueTreatsBlankAsAbsent(t *testing.T) {
	req := cervorules.Request{Metadata: map[string]string{"blank": "   ", "set": " 5 "}}

	if _, ok := factRawValue(req, "missing"); ok {
		t.Fatal("a missing key reported present")
	}
	if _, ok := factRawValue(req, "blank"); ok {
		t.Fatal("a blank value reported present")
	}
	raw, ok := factRawValue(req, "set")
	if !ok || raw != "5" {
		t.Fatalf("raw = %q ok = %v, want trimmed %q", raw, ok, "5")
	}
}

// A compiled matcher can fail — its leaves may consult the condition evaluator,
// not only the fact frame. A matcher that errors must fail the decision in both
// positions it can appear, the same way an unanswerable condition does. Neither
// position is reachable through the factory: no deny in policy-rules.yaml
// consults a condition, and no route declares a `when:` clause at all.
func TestAMatcherFailureFailsTheDecision(t *testing.T) {
	boom := errors.New("matcher could not decide")
	failing := func(context.Context, generatedEngine, cervorules.Request, factFrame) (bool, string, error) {
		return false, "", boom
	}

	t.Run("on a deny rule", func(t *testing.T) {
		engine := engineWithCondition(nil)
		route := engine.routes[cervorules.Operation("chat_completion")]
		route.requires = nil
		engine.routes[cervorules.Operation("chat_completion")] = route
		engine.denies = []generatedDeny{{
			operation: cervorules.Operation("chat_completion"),
			id:        "deny-with-matcher",
			reason:    "blocked by policy",
			match:     failing,
		}}

		result, err := engine.Decide(context.Background(), chatRequest())
		if !errors.Is(err, boom) {
			t.Fatalf("matcher failure not propagated: err=%v decision=%+v", err, result.Decision)
		}
		if result.Decision.Allow {
			t.Fatalf("it must not fall through to an allow: %+v", result.Decision)
		}
	})

	t.Run("on a route", func(t *testing.T) {
		engine := engineWithCondition(nil)
		route := engine.routes[cervorules.Operation("chat_completion")]
		route.requires = nil
		route.match = failing
		engine.routes[cervorules.Operation("chat_completion")] = route

		result, err := engine.Decide(context.Background(), chatRequest())
		if !errors.Is(err, boom) {
			t.Fatalf("matcher failure not propagated: err=%v decision=%+v", err, result.Decision)
		}
		if result.Decision.Allow {
			t.Fatalf("it must not fall through to an allow: %+v", result.Decision)
		}
	})
}

// A route whose `when:` predicate does not match is not eligible, and the
// denial says the conditions were not satisfied rather than pretending no route
// existed.
func TestARouteWhoseMatcherDoesNotMatchIsNotEligible(t *testing.T) {
	engine := engineWithCondition(nil)
	route := engine.routes[cervorules.Operation("chat_completion")]
	route.requires = nil
	route.match = func(context.Context, generatedEngine, cervorules.Request, factFrame) (bool, string, error) {
		return false, "body_bytes gt 1", nil
	}
	engine.routes[cervorules.Operation("chat_completion")] = route

	result, err := engine.DecideWithOptions(context.Background(), chatRequest(),
		cervorules.NewDecisionOptions(cervorules.WithTrace(true)))
	if err != nil {
		t.Fatalf("an unmatched predicate is a decision, not a fault: %v", err)
	}
	if result.Decision.Allow {
		t.Fatalf("route allowed with its predicate unmatched: %+v", result.Decision)
	}
	if !strings.Contains(result.Decision.Reason, "did not satisfy") {
		t.Fatalf("reason = %q", result.Decision.Reason)
	}
	step := result.Trace.Steps[len(result.Trace.Steps)-1]
	if step.Matched || step.Reason != "body_bytes gt 1" {
		t.Fatalf("the step must carry the leaf that decided: %+v", step)
	}
}

func TestBuildRejectsAConfigTheVocabularyDoesNotAccept(t *testing.T) {
	factory := NewPolicyFactory()
	config := factory.DefaultConfig()
	config.DefaultExecutor = cervorules.Executor("not_a_provider")
	if _, err := factory.Build(context.Background(), config); err == nil {
		t.Fatal("Build accepted an executor outside the vocabulary")
	}
}

// mergeRuntimeConfig has to create the maps it merges into. A base with none —
// which is what a hand-built config looks like — must not panic on the first
// override.
func TestMergeRuntimeConfigCreatesTheMapsItMergesInto(t *testing.T) {
	merged := mergeRuntimeConfig(cervoruntime.PolicyRuntimeConfig{}, cervoruntime.PolicyRuntimeConfig{
		OperationTargets:  map[cervorules.Operation]cervorules.Target{policyvocab.OperationPlanning: policyvocab.TargetCrashitoBrain},
		ExecutorFallbacks: map[cervorules.Executor][]cervorules.Executor{policyvocab.ExecutorOpenAI: {policyvocab.ExecutorOpenRouter}},
	})
	if merged.OperationTargets[policyvocab.OperationPlanning] != policyvocab.TargetCrashitoBrain {
		t.Fatalf("operation target lost: %+v", merged.OperationTargets)
	}
	if len(merged.ExecutorFallbacks[policyvocab.ExecutorOpenAI]) != 1 {
		t.Fatalf("executor fallbacks lost: %+v", merged.ExecutorFallbacks)
	}
}
