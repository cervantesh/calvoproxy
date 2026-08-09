package router

import "testing"

func TestInjectLocalAgentGuardrail_OnlyWhenToolsAndIdempotent(t *testing.T) {
	// No tools → no injection.
	plain := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hi"},
		},
	}
	injectLocalAgentGuardrail(plain)
	msgs := plain["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("no-tools request must not grow messages, got %d", len(msgs))
	}

	// Tools present → bookend system guards + tag last user turn.
	withTools := map[string]interface{}{
		"tools": []interface{}{
			map[string]interface{}{"type": "function"},
		},
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "list files"},
		},
	}
	injectLocalAgentGuardrail(withTools)
	msgs = withTools["messages"].([]interface{})
	// [system guard] + [user tagged] + [system guard]
	if len(msgs) != 3 {
		t.Fatalf("tools request should bookend with system guards, got %d msgs: %#v", len(msgs), msgs)
	}
	first := msgs[0].(map[string]interface{})
	last := msgs[len(msgs)-1].(map[string]interface{})
	if first["role"] != "system" || last["role"] != "system" {
		t.Fatalf("bookends must be system, got first=%v last=%v", first["role"], last["role"])
	}
	if !messageTextContains(first["content"], localAgentGuardrailMarker) {
		t.Fatalf("leading system missing marker: %v", first["content"])
	}
	user := msgs[1].(map[string]interface{})
	if !messageTextContains(user["content"], localAgentGuardrailMarker) {
		t.Fatalf("user turn should be tagged, got %v", user["content"])
	}

	// Second inject must not stack more copies (strip + re-bookend).
	injectLocalAgentGuardrail(withTools)
	if got := len(withTools["messages"].([]interface{})); got != 3 {
		t.Fatalf("second inject must stay bookended at 3 messages, got %d", got)
	}
}

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
