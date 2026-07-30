package observe

import "github.com/cervantesh/cervo-rules/v3/core"

const PolicyEvaluationReportSchemaVersion = "cervorules.v3.policy_evaluation_report.v1"

// PolicyEvaluationReport is the default v3 operational report shape.
type PolicyEvaluationReport struct {
	SchemaVersion string         `json:"schema_version"`
	RequestID     string         `json:"request_id,omitempty"`
	Operation     core.Operation `json:"operation,omitempty"`
	Allow         bool           `json:"allow"`
	Target        core.Target    `json:"target,omitempty"`
	Executor      core.Executor  `json:"executor,omitempty"`
	Reason        string         `json:"reason,omitempty"`
	Diagnostics   int            `json:"diagnostics,omitempty"`
}

// NewPolicyEvaluationReport builds a low-cardinality report from a decision.
func NewPolicyEvaluationReport(result core.DecisionResult) PolicyEvaluationReport {
	report := PolicyEvaluationReport{
		SchemaVersion: PolicyEvaluationReportSchemaVersion,
		Allow:         result.Decision.Allow,
		Target:        result.Decision.Target,
		Executor:      result.Decision.Executor,
		Reason:        result.Decision.Reason,
		Diagnostics:   len(result.Diagnostics),
	}
	if result.Observation != nil {
		report.RequestID = result.Observation.RequestID
		report.Operation = result.Observation.Operation
	}
	return report
}

// MetricLabels returns stable low-cardinality labels.
func (r PolicyEvaluationReport) MetricLabels() map[string]string {
	outcome := "deny"
	if r.Allow {
		outcome = "allow"
	}
	return map[string]string{
		"schema_version": r.SchemaVersion,
		"outcome":        outcome,
		"operation":      string(r.Operation),
		"target":         string(r.Target),
		"executor":       string(r.Executor),
	}
}

// LogFields returns structured logging fields without sensitive request data.
func (r PolicyEvaluationReport) LogFields() map[string]any {
	return map[string]any{
		"schema_version": r.SchemaVersion,
		"request_id":     r.RequestID,
		"operation":      r.Operation,
		"allow":          r.Allow,
		"target":         r.Target,
		"executor":       r.Executor,
		"reason":         r.Reason,
		"diagnostics":    r.Diagnostics,
	}
}
