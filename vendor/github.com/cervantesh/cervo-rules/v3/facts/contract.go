package facts

// TraceMode controls optional derivation trace materialization.
type TraceMode string

const (
	TraceDisabled TraceMode = "disabled"
	TraceEnabled  TraceMode = "enabled"
)

// EvalOptions requires explicit budgets for production facts evaluation.
type EvalOptions struct {
	MaxIterations                 int       `json:"max_iterations"`
	MaxFacts                      int       `json:"max_facts"`
	MaxBindings                   int       `json:"max_bindings,omitempty"`
	ExpensiveRuleBindingThreshold int       `json:"expensive_rule_binding_threshold,omitempty"`
	Trace                         TraceMode `json:"trace,omitempty"`
}

// Normalize applies v3 production defaults. Trace is opt-in.
func (o EvalOptions) Normalize() EvalOptions {
	if o.Trace == "" {
		o.Trace = TraceDisabled
	}
	return o
}

// TraceEnabled reports whether derivation traces should be materialized.
func (o EvalOptions) TraceEnabled() bool {
	return o.Normalize().Trace == TraceEnabled
}

// Atom identifies a predicate name.
type Atom string

// TermKind describes the kind of a fact or pattern term.
type TermKind string

const (
	TermConst TermKind = "const"
	TermVar   TermKind = "var"
)

// Term is a fact or pattern term.
type Term struct {
	Kind  TermKind `json:"kind"`
	Value string   `json:"value"`
}

// Fact is one materialized predicate tuple.
type Fact struct {
	Predicate Atom   `json:"predicate"`
	Terms     []Term `json:"terms,omitempty"`
}

// Pattern is a rule body or head pattern.
type Pattern struct {
	Predicate Atom   `json:"predicate"`
	Terms     []Term `json:"terms,omitempty"`
	Negated   bool   `json:"negated,omitempty"`
}

// Result is the v3 facts evaluation envelope.
type Result struct {
	Facts       []Fact                 `json:"facts,omitempty"`
	Plan        *EvaluationPlan        `json:"plan,omitempty"`
	Diagnostics []ComplexityDiagnostic `json:"diagnostics,omitempty"`
	Stats       EvaluationStats        `json:"stats,omitempty"`
}

// EvaluationStats carries machine-readable evaluation counters.
type EvaluationStats struct {
	Iterations int                  `json:"iterations,omitempty"`
	Rules      []RuleEvaluationStat `json:"rules,omitempty"`
}

// RuleEvaluationStat reports per-rule runtime cost.
type RuleEvaluationStat struct {
	Name         string `json:"name"`
	Evaluations  int    `json:"evaluations,omitempty"`
	DerivedFacts int    `json:"derived_facts,omitempty"`
	MaxBindings  int    `json:"max_bindings,omitempty"`
}

// ComplexityDiagnostic is the stable v3 facts diagnostic shape.
type ComplexityDiagnostic struct {
	Code     string `json:"code"`
	Rule     string `json:"rule,omitempty"`
	Field    string `json:"field,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Observed int    `json:"observed,omitempty"`
	Reason   string `json:"reason"`
}

// EvaluationPlan is a stable JSON contract for explaining evaluation cost.
type EvaluationPlan struct {
	SchemaVersion string     `json:"schema_version"`
	Rules         []RulePlan `json:"rules"`
}

// RulePlan describes one planned rule evaluation.
type RulePlan struct {
	Name      string        `json:"name"`
	Stratum   int           `json:"stratum"`
	Reordered bool          `json:"reordered"`
	Patterns  []PatternPlan `json:"patterns"`
}

// PatternPlan describes one planned body pattern.
type PatternPlan struct {
	DeclaredIndex     int    `json:"declared_index"`
	Predicate         string `json:"predicate"`
	Negated           bool   `json:"negated,omitempty"`
	Constants         int    `json:"constants"`
	ConstantPositions []int  `json:"constant_positions,omitempty"`
	CandidateCount    int    `json:"candidate_count"`
	SelectivityReason string `json:"selectivity_reason,omitempty"`
}

// NewEvaluationPlan creates a versioned plan value.
func NewEvaluationPlan(rules ...RulePlan) EvaluationPlan {
	return EvaluationPlan{
		SchemaVersion: "cervorules.v3.facts.plan.v1",
		Rules:         append([]RulePlan(nil), rules...),
	}
}
