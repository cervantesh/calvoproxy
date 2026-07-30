package core

// DecisionOption configures v3 decision evaluation or result construction.
type DecisionOption func(*DecisionOptions)

// DecisionOptions tunes evaluation work without changing decision semantics.
type DecisionOptions struct {
	trace       bool
	observation bool
}

// NewDecisionOptions applies functional options over production defaults.
func NewDecisionOptions(options ...DecisionOption) DecisionOptions {
	out := DecisionOptions{}
	for _, option := range options {
		if option != nil {
			option(&out)
		}
	}
	return out
}

// WithTrace enables or disables trace materialization.
func WithTrace(enabled bool) DecisionOption {
	return func(options *DecisionOptions) {
		options.trace = enabled
	}
}

// WithObservation enables or disables observation materialization.
func WithObservation(enabled bool) DecisionOption {
	return func(options *DecisionOptions) {
		options.observation = enabled
	}
}

// TraceEnabled reports whether trace storage should be materialized.
func (o DecisionOptions) TraceEnabled() bool {
	return o.trace
}

// ObservationEnabled reports whether observation storage should be materialized.
func (o DecisionOptions) ObservationEnabled() bool {
	return o.observation
}
