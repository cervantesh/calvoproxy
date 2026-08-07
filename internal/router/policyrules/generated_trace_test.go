package policyrules

import (
	"context"
	"testing"

	"github.com/cervantesh/calvoproxy/internal/router/policyvocab"
	cervorules "github.com/cervantesh/cervo-rules/v3/core"
)

// The rc.6 generator made the engine explain itself. These cover the paths that
// arrived with it, because a trace nobody tests is a trace nobody can trust.

func defaultEngine(t *testing.T) cervorules.Engine {
	t.Helper()
	factory := NewPolicyFactory()
	engine, err := factory.Build(context.Background(), factory.DefaultConfig())
	if err != nil {
		t.Fatalf("build default policy: %v", err)
	}
	return engine
}

func TestGeneratedPolicyExplainsAnAllowedRoute(t *testing.T) {
	result, err := defaultEngine(t).DecideWithOptions(context.Background(),
		cervorules.Request{Operation: policyvocab.OperationChatCompletion},
		cervorules.NewDecisionOptions(cervorules.WithTrace(true)))
	if err != nil {
		t.Fatalf("decide chat completion: %v", err)
	}
	if !result.Decision.Allow {
		t.Fatalf("chat completion denied: %+v", result.Decision)
	}
	if result.Trace == nil {
		t.Fatal("trace requested but not materialised")
	}
	if len(result.Trace.Steps) == 0 {
		t.Fatal("an allowed route must report the rule that matched")
	}
	step := result.Trace.Steps[len(result.Trace.Steps)-1]
	if !step.Matched {
		t.Fatalf("the route that produced the allow reports Matched=false: %+v", step)
	}
	if step.Name == "" {
		t.Fatalf("trace step carries no rule id: %+v", step)
	}
}

// The gate the whole subsystem rests on: asking for no trace must materialise
// nothing, so tracing costs nothing when it is off.
func TestGeneratedPolicyMaterialisesNoTraceWhenNotAsked(t *testing.T) {
	result, err := defaultEngine(t).DecideWithOptions(context.Background(),
		cervorules.Request{Operation: policyvocab.OperationChatCompletion},
		cervorules.NewDecisionOptions())
	if err != nil {
		t.Fatalf("decide chat completion: %v", err)
	}
	if !result.Decision.Allow {
		t.Fatalf("chat completion denied: %+v", result.Decision)
	}
	if result.Trace != nil {
		t.Fatalf("trace materialised without being asked for: %+v", result.Trace)
	}
}

// A denial has to be as explicable as an allow — a trace that only covers the
// happy path is worthless exactly when someone needs it.
func TestGeneratedPolicyExplainsADenial(t *testing.T) {
	result, err := defaultEngine(t).DecideWithOptions(context.Background(),
		cervorules.Request{Operation: cervorules.Operation("unknown_operation")},
		cervorules.NewDecisionOptions(cervorules.WithTrace(true)))
	if err != nil {
		t.Fatalf("decide unknown operation: %v", err)
	}
	if result.Decision.Allow {
		t.Fatalf("unknown operation allowed: %+v", result.Decision)
	}
	if result.Trace == nil {
		t.Fatal("a denial must be traceable")
	}
	if result.Decision.Reason == "" {
		t.Fatal("a denial with no reason is not a decision anyone can act on")
	}
}

func TestGeneratedPolicyHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := defaultEngine(t).DecideWithOptions(ctx,
		cervorules.Request{Operation: policyvocab.OperationChatCompletion},
		cervorules.NewDecisionOptions()); err == nil {
		t.Fatal("a cancelled context must not produce a decision")
	}
}

// isTrustedUser is the gate that CalvoProxy stopped feeding from an
// unauthenticated header. Both sides of it are pinned here: the engine grants
// the route to a configured trusted user, and normalises case and surrounding
// space when deciding who that is.
func TestGeneratedPolicyTrustedUserGate(t *testing.T) {
	factory := NewPolicyFactory()
	config := factory.DefaultConfig()
	config.TrustedUsers = []string{"cervantes"}
	engine, err := factory.Build(context.Background(), config)
	if err != nil {
		t.Fatalf("build with trusted users: %v", err)
	}

	for _, user := range []string{"cervantes", "Cervantes", "  cervantes  "} {
		result, err := engine.Decide(context.Background(), cervorules.Request{
			User:      user,
			Operation: policyvocab.OperationSecretLookup,
		})
		if err != nil {
			t.Fatalf("decide for %q: %v", user, err)
		}
		if !result.Decision.Allow {
			t.Fatalf("trusted user %q denied: %+v", user, result.Decision)
		}
	}

	for _, user := range []string{"", "   ", "guest", "cervantes2"} {
		result, err := engine.Decide(context.Background(), cervorules.Request{
			User:      user,
			Operation: policyvocab.OperationSecretLookup,
		})
		if err != nil {
			t.Fatalf("decide for %q: %v", user, err)
		}
		if result.Decision.Allow {
			t.Fatalf("untrusted user %q allowed: %+v", user, result.Decision)
		}
	}
}
