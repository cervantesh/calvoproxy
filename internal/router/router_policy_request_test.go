package router

import "testing"

func TestRequestPolicyFactsExtractsRequestedLimitsAndOperationalMetadata(t *testing.T) {
	tests := []struct {
		name          string
		maxTokens     any
		wantMaxTokens int
	}{
		{name: "float max_tokens", maxTokens: float64(42), wantMaxTokens: 42},
		{name: "int max_tokens", maxTokens: 43, wantMaxTokens: 43},
		{name: "string max_tokens", maxTokens: "44", wantMaxTokens: 44},
		{name: "empty string max_tokens", maxTokens: "", wantMaxTokens: 0},
		{name: "invalid string max_tokens", maxTokens: "many", wantMaxTokens: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := map[string]interface{}{"max_tokens": tt.maxTokens}
			facts := requestPolicyFacts(body, "coding", "model-a", true, true, true, 123)

			if facts.Metadata["profile"] != "coding" || facts.Metadata["requested_model"] != "model-a" {
				t.Fatalf("expected operational metadata, got %+v", facts.Metadata)
			}
			want := RequestedLimits{
				BodyBytes: 123,
				MaxTokens: tt.wantMaxTokens,
				Stream:    true,
				Tools:     true,
				Images:    true,
			}
			if facts.RequestedLimits != want {
				t.Fatalf("expected requested limits %+v, got %+v", want, facts.RequestedLimits)
			}
		})
	}
}
