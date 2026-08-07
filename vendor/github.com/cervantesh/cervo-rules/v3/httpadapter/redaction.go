package httpadapter

import "github.com/cervantesh/cervo-rules/v3/core"

// SensitiveFieldNames are the transport-shaped error field names this adapter
// owns. They live here rather than in core because "header" and "body" only
// mean something once a consumer has chosen HTTP.
func SensitiveFieldNames() []string {
	return []string{"authorization", "auth", "header", "headers", "body", "cookie", "credential", "token"}
}

// Redactor withholds values for transport-shaped error fields.
//
// Compose it with any other Redactor a consumer needs:
//
//	errs.RedactWith(httpadapter.Redactor())
func Redactor() core.Redactor {
	return core.RedactFields(SensitiveFieldNames()...)
}
