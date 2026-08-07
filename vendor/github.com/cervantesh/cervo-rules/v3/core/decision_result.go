package core

// Schema versions make v3 runtime envelopes machine-readable.
const (
	DecisionTraceSchemaVersion       = "cervorules.v3.decision_trace.v1"
	DecisionObservationSchemaVersion = "cervorules.v3.decision_observation.v1"
)

// DecisionResult is the v3 runtime envelope returned by decision engines.
type DecisionResult struct {
	Decision    Decision
	Trace       *DecisionTrace
	Observation *DecisionObservation
	Diagnostics Errors
	Stats       DecisionStats
}

// DecisionTrace contains opt-in runtime explanation data.
type DecisionTrace struct {
	SchemaVersion string
	Steps         []DecisionTraceStep
}

// DecisionTraceStep records a single machine-readable evaluation step.
type DecisionTraceStep struct {
	Name    string
	Matched bool
	Reason  string
}

// DecisionObservation is the default low-cardinality operational report.
type DecisionObservation struct {
	SchemaVersion string
	RequestID     string
	Operation     Operation
	Allow         bool
	Target        Target
	Executor      Executor
	Reason        string

	// User intentionally stays empty unless a future explicit opt-in allows it.
	User string
}

// DecisionStats carries cheap counters without requiring trace storage.
type DecisionStats struct {
	TraceEnabled       bool
	ObservationEnabled bool
	Diagnostics        int
}

// NewDecisionResult creates a v3 decision envelope with production-oriented
// defaults: trace and observation are opt-in.
func NewDecisionResult(req Request, decision Decision, options ...DecisionOption) DecisionResult {
	decisionOptions := NewDecisionOptions(options...)
	result := DecisionResult{
		Decision: decision,
		Stats: DecisionStats{
			TraceEnabled:       decisionOptions.TraceEnabled(),
			ObservationEnabled: decisionOptions.ObservationEnabled(),
		},
	}
	if decisionOptions.TraceEnabled() {
		result.Trace = &DecisionTrace{SchemaVersion: DecisionTraceSchemaVersion}
	}
	if decisionOptions.ObservationEnabled() {
		result.Observation = &DecisionObservation{
			SchemaVersion: DecisionObservationSchemaVersion,
			RequestID:     req.ID,
			Operation:     req.Operation,
			Allow:         decision.Allow,
			Target:        decision.Target,
			Executor:      decision.Executor,
			Reason:        decision.Reason,
		}
	}
	return result
}
