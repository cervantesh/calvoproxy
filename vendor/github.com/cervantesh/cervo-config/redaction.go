package configenv

// RedactedValue is used when rendering sensitive defaults or examples.
const RedactedValue = "[REDACTED]"

// RedactVars copies vars and redacts defaults marked as sensitive.
func RedactVars(vars []Var) []Var {
	redacted := append([]Var(nil), vars...)
	for i := range redacted {
		if redacted[i].Sensitive && redacted[i].Default != "" {
			redacted[i].Default = RedactedValue
		}
	}
	return redacted
}

// RedactFields copies fields and redacts defaults marked as sensitive.
func RedactFields(fields []Field) []Field {
	redacted := append([]Field(nil), fields...)
	for i := range redacted {
		if redacted[i].Sensitive && redacted[i].Default != "" {
			redacted[i].Default = RedactedValue
		}
	}
	return redacted
}
