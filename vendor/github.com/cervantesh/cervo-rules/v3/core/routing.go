package core

// RoutingStrategy names the explicit routing behavior used by a policy.
type RoutingStrategy string

const (
	// IndexedRoutingStrategy is the default v3 policy-builder routing path.
	IndexedRoutingStrategy RoutingStrategy = "indexed"
	// LinearRoutingStrategy is available only when a caller intentionally wants
	// ordered O(n) rule evaluation.
	LinearRoutingStrategy RoutingStrategy = "linear"
)

// RoutingRule maps an operation to a target/executor pair.
type RoutingRule struct {
	Operation Operation `json:"operation"`
	Target    Target    `json:"target"`
	Executor  Executor  `json:"executor,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// RoutingPlan describes routing shape without hiding cost.
type RoutingPlan struct {
	Strategy RoutingStrategy `json:"strategy"`
	Rules    []RoutingRule   `json:"rules,omitempty"`
}

// NewIndexedRoutingPlan is the v3 default for operation routing.
func NewIndexedRoutingPlan(rules ...RoutingRule) RoutingPlan {
	return RoutingPlan{Strategy: IndexedRoutingStrategy, Rules: append([]RoutingRule(nil), rules...)}
}

// NewLinearRoutingPlan makes O(n) ordered routing explicit.
func NewLinearRoutingPlan(rules ...RoutingRule) RoutingPlan {
	return RoutingPlan{Strategy: LinearRoutingStrategy, Rules: append([]RoutingRule(nil), rules...)}
}

// IsLinear reports whether this routing plan has O(n) route selection behavior.
func (p RoutingPlan) IsLinear() bool {
	return p.Strategy == LinearRoutingStrategy
}
