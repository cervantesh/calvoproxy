package runtime

import (
	"strings"

	"github.com/cervantesh/cervo-rules/v3/core"
)

// PolicyBuildError wraps generated policy construction failures with policy
// metadata so machines and CI can identify the failing generated artifact.
type PolicyBuildError struct {
	Metadata PolicyMetadata `json:"metadata"`
	Errors   core.Errors    `json:"errors"`
	Cause    error          `json:"-"`
}

// NewPolicyBuildError creates a metadata-bearing policy build error.
func NewPolicyBuildError(metadata PolicyMetadata, cause error) *PolicyBuildError {
	if cause == nil {
		return nil
	}
	return &PolicyBuildError{
		Metadata: metadata,
		Errors:   normalizePolicyBuildErrors(cause),
		Cause:    cause,
	}
}

func (e *PolicyBuildError) Error() string {
	if e == nil {
		return "policy build error"
	}
	parts := []string{"policy build failed"}
	if e.Metadata.Name != "" {
		parts = append(parts, e.Metadata.Name)
	}
	if len(e.Errors) > 0 {
		parts = append(parts, e.Errors.Error())
	}
	return strings.Join(parts, ": ")
}

// Unwrap exposes the original source error for errors.Is.
func (e *PolicyBuildError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// As lets errors.As extract the wrapped structured error collection.
func (e *PolicyBuildError) As(target any) bool {
	if e == nil {
		return false
	}
	switch typed := target.(type) {
	case *core.Errors:
		*typed = e.Errors
		return true
	case *core.Error:
		if len(e.Errors) == 0 {
			return false
		}
		*typed = e.Errors[0]
		return true
	default:
		return false
	}
}

func normalizePolicyBuildErrors(cause error) core.Errors {
	switch typed := cause.(type) {
	case core.Errors:
		return typed.SortStable()
	case core.Error:
		return core.Errors{typed}
	default:
		return core.Errors{{
			Code:      core.ErrorCodePolicyBuildFailed,
			Severity:  core.SeverityFatal,
			Component: "runtime",
			Reason:    "policy build failed",
		}}
	}
}
