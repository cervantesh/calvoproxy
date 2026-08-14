package policyrules

import (
	"context"
	"testing"

	"github.com/cervantesh/calvoproxy/internal/router/policyvocab"
	cervorules "github.com/cervantesh/cervo-rules/v3/core"
	cervoruntime "github.com/cervantesh/cervo-rules/v3/runtime"
)

func TestGeneratedRuntimePolicyContract(t *testing.T) {
	factory := NewPolicyFactory()
	cfg := factory.DefaultConfig()
	cfg.DefaultExecutor = policyvocab.ExecutorOpenAI
	cfg.OperationTargets[policyvocab.OperationPlanning] = policyvocab.TargetCrashitoBrain
	cfg.OperationTargets[policyvocab.OperationMediaRequest] = policyvocab.TargetCervoMedia
	cfg.ExecutorFallbacks[policyvocab.ExecutorOpenAI] = []cervorules.Executor{policyvocab.ExecutorOpenRouter}

	if err := factory.ValidateConfig(cfg); err != nil {
		t.Fatalf("validate override: %v", err)
	}
	engine, err := factory.Build(context.Background(), cfg)
	if err != nil {
		t.Fatalf("build override: %v", err)
	}

	planning, err := engine.Decide(context.Background(), cervorules.Request{Operation: policyvocab.OperationPlanning})
	if err != nil {
		t.Fatalf("decide planning: %v", err)
	}
	if !planning.Decision.Allow || planning.Decision.Target != policyvocab.TargetCrashitoBrain {
		t.Fatalf("unexpected planning decision: %+v", planning.Decision)
	}

	media, err := engine.Decide(context.Background(), cervorules.Request{Operation: policyvocab.OperationMediaRequest})
	if err != nil {
		t.Fatalf("decide media: %v", err)
	}
	if !media.Decision.Allow || media.Decision.Target != policyvocab.TargetCervoMedia {
		t.Fatalf("generated policy should enable media through runtime target override: %+v", media.Decision)
	}

	if media.Decision.Executor != policyvocab.ExecutorOpenAI {
		t.Fatalf("expected generated policy to apply default executor override, got %+v", media.Decision)
	}
	if len(media.Decision.FallbackExecutors) != 1 || media.Decision.FallbackExecutors[0] != policyvocab.ExecutorOpenRouter {
		t.Fatalf("expected generated policy fallback executors, got %+v", media.Decision.FallbackExecutors)
	}
}

func TestValidateRuntimeConfigReportsGeneratedVocabularyErrors(t *testing.T) {
	err := NewPolicyFactory().ValidateConfig(cervoruntime.PolicyRuntimeConfig{
		ExecutorFallbacks: map[cervorules.Executor][]cervorules.Executor{
			policyvocab.ExecutorOpenAI: {cervorules.Executor("unknown_provider")},
		},
	})
	if err == nil {
		t.Fatal("expected invalid generated runtime config to fail")
	}
}

func TestGeneratedRuntimePolicyDeniesUntrustedSecretLookup(t *testing.T) {
	factory := NewPolicyFactory()
	engine, err := factory.Build(context.Background(), factory.DefaultConfig())
	if err != nil {
		t.Fatalf("build default policy: %v", err)
	}

	result, err := engine.Decide(context.Background(), cervorules.Request{
		User:      "guest",
		Operation: policyvocab.OperationSecretLookup,
	})
	if err != nil {
		t.Fatalf("decide secret lookup: %v", err)
	}
	if result.Decision.Allow {
		t.Fatalf("expected untrusted secret lookup to be denied: %+v", result.Decision)
	}
}

func TestGeneratedRuntimePolicyDeniesUnknownOperation(t *testing.T) {
	factory := NewPolicyFactory()
	engine, err := factory.Build(context.Background(), factory.DefaultConfig())
	if err != nil {
		t.Fatalf("build default policy: %v", err)
	}

	result, err := engine.Decide(context.Background(), cervorules.Request{
		Operation: cervorules.Operation("unknown_operation"),
	})
	if err != nil {
		t.Fatalf("decide unknown operation: %v", err)
	}
	if result.Decision.Allow {
		t.Fatalf("expected unknown operation to be denied: %+v", result.Decision)
	}
}
