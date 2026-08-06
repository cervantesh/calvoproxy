package limits

import "fmt"

// Budget is a cross-domain limit budget. Consumers own how request payloads
// are transformed into Requested values.
type Budget struct {
	MaxBodyBytes int64 `json:"max_body_bytes,omitempty"`
	MaxTokens    int   `json:"max_tokens,omitempty"`
	AllowStream  bool  `json:"allow_stream,omitempty"`
	AllowTools   bool  `json:"allow_tools,omitempty"`
	AllowImages  bool  `json:"allow_images,omitempty"`
}

// Requested describes the limit-relevant shape of an input.
type Requested struct {
	BodyBytes int64
	MaxTokens int
	Stream    bool
	Tools     bool
	Images    bool
}

// Violation is one stable, machine-readable budget violation.
type Violation struct {
	Field  string
	Reason string
}

func (v Violation) Error() string { return v.Reason }

// Violations contains all violations found for a request.
type Violations []Violation

func (v Violations) Error() string {
	if len(v) == 0 {
		return ""
	}
	return v[0].Error()
}

// Has reports whether a violation for field was returned.
func (v Violations) Has(field string) bool {
	for _, violation := range v {
		if violation.Field == field {
			return true
		}
	}
	return false
}

// Check compares requested usage with a budget and returns every violation.
func Check(budget Budget, requested Requested) Violations {
	var violations Violations
	if budget.MaxBodyBytes > 0 && requested.BodyBytes > budget.MaxBodyBytes {
		violations = append(violations, Violation{Field: "body_bytes", Reason: fmt.Sprintf("request body exceeds policy limit of %d bytes", budget.MaxBodyBytes)})
	}
	if budget.MaxTokens > 0 && requested.MaxTokens > budget.MaxTokens {
		violations = append(violations, Violation{Field: "max_tokens", Reason: fmt.Sprintf("max_tokens exceeds policy limit of %d", budget.MaxTokens)})
	}
	if requested.Stream && !budget.AllowStream {
		violations = append(violations, Violation{Field: "stream", Reason: "streaming is not allowed by policy"})
	}
	if requested.Tools && !budget.AllowTools {
		violations = append(violations, Violation{Field: "tools", Reason: "tools are not allowed by policy"})
	}
	if requested.Images && !budget.AllowImages {
		violations = append(violations, Violation{Field: "images", Reason: "images are not allowed by policy"})
	}
	return violations
}
