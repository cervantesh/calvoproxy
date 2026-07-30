package core

import (
	"encoding/json"
	"sort"
	"strings"
)

// ErrorCode is a stable machine-readable v3 error identifier.
type ErrorCode string

const (
	ErrorCodeInvalidRule        ErrorCode = "invalid_rule"
	ErrorCodeInvalidConfig      ErrorCode = "invalid_config"
	ErrorCodeEmptyOperation     ErrorCode = "empty_operation"
	ErrorCodeUnknownOperation   ErrorCode = "unknown_operation"
	ErrorCodeEmptyTarget        ErrorCode = "empty_target"
	ErrorCodeUnknownTarget      ErrorCode = "unknown_target"
	ErrorCodeEmptyExecutor      ErrorCode = "empty_executor"
	ErrorCodeUnknownExecutor    ErrorCode = "unknown_executor"
	ErrorCodeRenamedPrimitive   ErrorCode = "renamed_primitive"
	ErrorCodeUnsupportedFeature ErrorCode = "unsupported_feature"
	ErrorCodeDeprecatedField    ErrorCode = "deprecated_field"

	ErrorCodeInvalidRuntimeConfig ErrorCode = "invalid_runtime_config"

	ErrorCodeInvalidPolicySchema     ErrorCode = "invalid_policy_schema"
	ErrorCodeSchemaValidationFailed  ErrorCode = "schema_validation_failed"
	ErrorCodePolicyBuildFailed       ErrorCode = "policy_build_failed"
	ErrorCodeGeneratedPolicyInvalid  ErrorCode = "generated_policy_invalid"
	ErrorCodeDeprecatedGeneratedAPI  ErrorCode = "deprecated_generated_api"
	ErrorCodeCompatBreakingChange    ErrorCode = "compat_breaking_change"
	ErrorCodeInternalInvariantFailed ErrorCode = "internal_invariant_failed"

	ErrorCodeEvaluationFailed        ErrorCode = "evaluation_failed"
	ErrorCodeContextCanceled         ErrorCode = "context_canceled"
	ErrorCodeContextDeadlineExceeded ErrorCode = "context_deadline_exceeded"
	ErrorCodeBudgetExceeded          ErrorCode = "budget_exceeded"
	ErrorCodeUnsafeRule              ErrorCode = "unsafe_rule"
	ErrorCodeUnsafeNegation          ErrorCode = "unsafe_negation"
	ErrorCodeMaxFactsExceeded        ErrorCode = "max_facts_exceeded"
	ErrorCodeMaxBindingsExceeded     ErrorCode = "max_bindings_exceeded"
	ErrorCodeMaxIterationsExceeded   ErrorCode = "max_iterations_exceeded"
	ErrorCodeExpensiveRule           ErrorCode = "expensive_rule"
	ErrorCodeRuleDisabled            ErrorCode = "rule_disabled"

	ErrorCodeBodyBytesExceeded ErrorCode = "body_bytes_exceeded"
	ErrorCodeMaxTokensExceeded ErrorCode = "max_tokens_exceeded"
	ErrorCodeStreamNotAllowed  ErrorCode = "stream_not_allowed"
	ErrorCodeToolsNotAllowed   ErrorCode = "tools_not_allowed"
	ErrorCodeImagesNotAllowed  ErrorCode = "images_not_allowed"
)

// Severity is a stable machine-readable severity for structured errors.
type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
	SeverityFatal   Severity = "fatal"
)

// Error is the common v3 structured error shape.
type Error struct {
	Code       ErrorCode `json:"code"`
	Severity   Severity  `json:"severity,omitempty"`
	Component  string    `json:"component,omitempty"`
	Field      string    `json:"field,omitempty"`
	Rule       string    `json:"rule,omitempty"`
	Value      string    `json:"value,omitempty"`
	Limit      int       `json:"limit,omitempty"`
	Observed   int       `json:"observed,omitempty"`
	Reason     string    `json:"reason"`
	Suggestion string    `json:"suggestion,omitempty"`
	Cause      error     `json:"-"`
}

// MarshalJSON redacts sensitive Value fields by default while preserving the
// source error Cause only for errors.Is / errors.As.
func (e Error) MarshalJSON() ([]byte, error) {
	type wireError Error
	redacted := wireError(e.Redacted())
	return json.Marshal(redacted)
}

// Redacted returns a copy with sensitive user-provided values removed.
func (e Error) Redacted() Error {
	if e.Value != "" && isSensitiveErrorField(e.Field) {
		e.Value = "[REDACTED]"
	}
	return e
}

func (e Error) Error() string {
	parts := make([]string, 0, 6)
	if e.Code != "" {
		parts = append(parts, string(e.Code))
	}
	if e.Component != "" {
		parts = append(parts, e.Component)
	}
	if e.Field != "" {
		parts = append(parts, e.Field)
	}
	if e.Rule != "" {
		parts = append(parts, e.Rule)
	}
	if e.Value != "" {
		parts = append(parts, e.Value)
	}
	if e.Reason != "" {
		parts = append(parts, e.Reason)
	}
	if e.Suggestion != "" {
		parts = append(parts, e.Suggestion)
	}
	if len(parts) == 0 {
		return "cervorules error"
	}
	return strings.Join(parts, ": ")
}

// Unwrap exposes an optional source error for errors.Is / errors.As.
func (e Error) Unwrap() error {
	return e.Cause
}

func isSensitiveErrorField(field string) bool {
	normalized := strings.ToLower(strings.TrimSpace(field))
	for _, sensitive := range []string{"token", "auth", "header", "body", "prompt", "content", "memory"} {
		if strings.Contains(normalized, sensitive) {
			return true
		}
	}
	return false
}

// Errors is a multi-error collection with stable code lookup.
type Errors []Error

// Redacted returns a copy with sensitive user-provided values removed.
func (e Errors) Redacted() Errors {
	out := make(Errors, 0, len(e))
	for _, err := range e {
		out = append(out, err.Redacted())
	}
	return out
}

func (e Errors) Error() string {
	if len(e) == 0 {
		return "no validation errors"
	}
	if len(e) == 1 {
		return e[0].Error()
	}
	return e[0].Error() + " and more validation errors"
}

// As lets errors.As extract either the whole collection or the first item.
func (e Errors) As(target any) bool {
	switch typed := target.(type) {
	case *Errors:
		*typed = e
		return true
	case *Error:
		if len(e) == 0 {
			return false
		}
		*typed = e[0]
		return true
	default:
		return false
	}
}

// Has reports whether the collection contains an error with code.
func (e Errors) Has(code ErrorCode) bool {
	for _, err := range e {
		if err.Code == code {
			return true
		}
	}
	return false
}

// ByCode returns all errors with code while preserving their collection order.
func (e Errors) ByCode(code ErrorCode) Errors {
	filtered := make(Errors, 0, len(e))
	for _, err := range e {
		if err.Code == code {
			filtered = append(filtered, err)
		}
	}
	return filtered
}

// ByField returns all errors with field while preserving their collection order.
func (e Errors) ByField(field string) Errors {
	filtered := make(Errors, 0, len(e))
	for _, err := range e {
		if err.Field == field {
			filtered = append(filtered, err)
		}
	}
	return filtered
}

// Fatal returns all fatal errors while preserving their collection order.
func (e Errors) Fatal() Errors {
	filtered := make(Errors, 0, len(e))
	for _, err := range e {
		if err.Severity == SeverityFatal {
			filtered = append(filtered, err)
		}
	}
	return filtered
}

// SortStable returns a deterministic sorted copy of the collection.
func (e Errors) SortStable() Errors {
	sorted := append(Errors(nil), e...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return errorSortKey(sorted[i]) < errorSortKey(sorted[j])
	})
	return sorted
}

func errorSortKey(err Error) string {
	return strings.Join([]string{
		err.Field,
		err.Rule,
		string(err.Code),
		err.Value,
		err.Reason,
	}, "\x00")
}

// ErrInvalidRule is retained as a named v3 error value for rule validation.
var ErrInvalidRule = Error{Code: ErrorCodeInvalidRule, Reason: "invalid rule"}
