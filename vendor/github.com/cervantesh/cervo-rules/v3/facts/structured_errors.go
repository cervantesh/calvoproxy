package facts

import "github.com/cervantesh/cervo-rules/v3/core"

// StructuredErrorsFromDiagnostics converts fatal facts diagnostics to core
// structured errors while preserving non-fatal diagnostics for callers to
// inspect separately.
func StructuredErrorsFromDiagnostics(diagnostics []ComplexityDiagnostic) core.Errors {
	errs := make(core.Errors, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		if err, ok := StructuredErrorFromDiagnostic(diagnostic); ok {
			errs = append(errs, err)
		}
	}
	return errs.SortStable()
}

// StructuredErrorFromDiagnostic converts one fatal facts diagnostic to a core
// error. It returns ok=false for non-fatal diagnostics such as expensive_rule.
func StructuredErrorFromDiagnostic(diagnostic ComplexityDiagnostic) (core.Error, bool) {
	code, ok := fatalDiagnosticCode(diagnostic.Code)
	if !ok {
		return core.Error{}, false
	}
	return core.Error{
		Code:      code,
		Severity:  core.SeverityFatal,
		Component: "facts",
		Field:     diagnostic.Field,
		Rule:      diagnostic.Rule,
		Limit:     diagnostic.Limit,
		Observed:  diagnostic.Observed,
		Reason:    diagnostic.Reason,
	}, true
}

func fatalDiagnosticCode(code string) (core.ErrorCode, bool) {
	switch code {
	case "max_bindings":
		return core.ErrorCodeMaxBindingsExceeded, true
	case "max_facts":
		return core.ErrorCodeMaxFactsExceeded, true
	case "max_iterations":
		return core.ErrorCodeMaxIterationsExceeded, true
	case "budget_exceeded":
		return core.ErrorCodeBudgetExceeded, true
	case "unsafe_negation":
		return core.ErrorCodeUnsafeNegation, true
	case "unsafe_rule":
		return core.ErrorCodeUnsafeRule, true
	default:
		return "", false
	}
}
