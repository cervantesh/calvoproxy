package core

import "context"

// Engine is the v3 context-aware runtime contract.
type Engine interface {
	Decide(context.Context, Request) (DecisionResult, error)
	DecideWithOptions(context.Context, Request, DecisionOptions) (DecisionResult, error)
}
