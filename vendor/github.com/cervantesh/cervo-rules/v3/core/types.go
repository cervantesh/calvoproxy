package core

import "strings"

// Operation identifies what a request is trying to do.
type Operation string

// Target identifies the logical destination selected by a decision.
type Target string

// Executor identifies the caller-owned execution choice selected by a decision.
type Executor string

// Request is the normalized input evaluated by v3 decision runtimes.
type Request struct {
	ID        string
	User      string
	Channel   string
	Operation Operation
	Metadata  map[string]string
}

// Decision is the policy outcome returned by v3 decision runtimes.
type Decision struct {
	Allow             bool
	Target            Target
	Executor          Executor
	FallbackTargets   []Target
	FallbackExecutors []Executor
	Reason            string
}

// NewOperation normalizes a caller-owned operation name.
func NewOperation(value string) Operation { return Operation(normalize(value)) }

// NewTarget normalizes a caller-owned target name.
func NewTarget(value string) Target { return Target(normalize(value)) }

// NewExecutor normalizes a caller-owned executor name.
func NewExecutor(value string) Executor { return Executor(normalize(value)) }

func normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
