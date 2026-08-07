package runtime

import (
	"context"

	"github.com/cervantesh/cervo-rules/v3/core"
)

// PolicyFactory is the canonical v3 generated policy entrypoint.
type PolicyFactory interface {
	DefaultConfig() PolicyRuntimeConfig
	ValidateConfig(PolicyRuntimeConfig) error
	Build(context.Context, PolicyRuntimeConfig) (core.Engine, error)
	Metadata() PolicyMetadata
}

// PolicyMetadata describes a generated policy for machines and release tooling.
type PolicyMetadata struct {
	Name           string
	DSLVersion     string
	GeneratedWith  string
	VocabularyHash string
	PolicyHash     string
}

// PolicyRuntimeConfig is the v3 generated-policy runtime override contract.
type PolicyRuntimeConfig struct {
	TrustedUsers      []string
	DefaultExecutor   core.Executor
	OperationTargets  map[core.Operation]core.Target
	ExecutorFallbacks map[core.Executor][]core.Executor

	// Conditions resolves the named guards a policy consults before allowing an
	// operation. A nil value means the policy declares no condition-gated rules;
	// a policy that does declare them must be built with an evaluator, and the
	// generated ValidateConfig rejects the combination when it is missing.
	//
	// Implementations must be side-effect free: a decision may consult the same
	// condition more than once, and nothing may happen before the decision is
	// returned.
	Conditions core.Conditions
}

// Clone defensively copies mutable config fields.
//
// Conditions is carried by reference on purpose: an evaluator is a caller-owned
// collaborator, not config data, and it is required to be side-effect free.
func (c PolicyRuntimeConfig) Clone() PolicyRuntimeConfig {
	out := c
	out.TrustedUsers = append([]string(nil), c.TrustedUsers...)
	if c.OperationTargets != nil {
		out.OperationTargets = make(map[core.Operation]core.Target, len(c.OperationTargets))
		for operation, target := range c.OperationTargets {
			out.OperationTargets[operation] = target
		}
	}
	if c.ExecutorFallbacks != nil {
		out.ExecutorFallbacks = make(map[core.Executor][]core.Executor, len(c.ExecutorFallbacks))
		for executor, fallbacks := range c.ExecutorFallbacks {
			out.ExecutorFallbacks[executor] = append([]core.Executor(nil), fallbacks...)
		}
	}
	return out
}
