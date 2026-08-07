package core

import "strings"

// RedactedValue is the placeholder substituted for a withheld error value.
const RedactedValue = "[REDACTED]"

// Redactor reports whether the value carried by a structured error field must
// be withheld from serialization.
//
// core deliberately ships no built-in field vocabulary. Which field names are
// sensitive is a property of the consumer's domain, not of policy evaluation.
// Each adapter or consumer declares its own names; core only provides the
// mechanism for applying them.
type Redactor func(field string) bool

// RedactFields builds a Redactor that matches whole field-name segments.
//
// Field names are split on ".", "_", "[" and "]", and each segment is compared
// exactly. Segment matching avoids the false positives that substring matching
// produces: a field named "max_count" is not withheld merely because a caller
// asked to redact "count" values elsewhere, while a compound field such as
// "account_secret" still matches the segment "secret".
func RedactFields(names ...string) Redactor {
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		if normalized := normalize(name); normalized != "" {
			wanted[normalized] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return func(string) bool { return false }
	}
	return func(field string) bool {
		for _, segment := range fieldSegments(field) {
			if _, ok := wanted[segment]; ok {
				return true
			}
		}
		return false
	}
}

func fieldSegments(field string) []string {
	normalized := normalize(field)
	if normalized == "" {
		return nil
	}
	segments := strings.FieldsFunc(normalized, func(r rune) bool {
		return r == '.' || r == '_' || r == '[' || r == ']'
	})
	return append(segments, normalized)
}
