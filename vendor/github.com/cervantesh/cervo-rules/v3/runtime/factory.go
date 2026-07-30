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
}

// Clone defensively copies mutable config fields.
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
