package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestBodyForAttempt_StripsOpenCodeReasoningForDirectProviders(t *testing.T) {
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "assistant", "content": "answer", "reasoning_content": "private trace"},
		},
	}

	groq := requestBodyForAttempt(body, modelAttempt{Provider: providerGroq})
	groqMessage := groq["messages"].([]interface{})[0].(map[string]interface{})
	if _, exists := groqMessage["reasoning_content"]; exists {
		t.Fatalf("Groq request must not include reasoning_content: %#v", groqMessage)
	}
	if got := body["messages"].([]interface{})[0].(map[string]interface{})["reasoning_content"]; got != "private trace" {
		t.Fatalf("shared request was mutated: %#v", body)
	}
	cerebras := requestBodyForAttempt(body, modelAttempt{Provider: providerCerebras})
	if _, exists := cerebras["messages"].([]interface{})[0].(map[string]interface{})["reasoning_content"]; exists {
		t.Fatalf("Cerebras request must not include reasoning_content: %#v", cerebras)
	}
	if got := cerebras["messages"].([]interface{})[0].(map[string]interface{})["reasoning"]; got != "private trace" {
		t.Fatalf("Cerebras request should preserve reasoning under provider field, got %#v", got)
	}

	openRouter := requestBodyForAttempt(body, modelAttempt{Provider: providerOpenRouter})
	if got := openRouter["messages"].([]interface{})[0].(map[string]interface{})["reasoning_content"]; got != "private trace" {
		t.Fatalf("OpenRouter request should retain reasoning_content, got %#v", openRouter)
	}
}

func TestRequestBodyForAttemptAppliesProviderContractsWithoutMutatingSource(t *testing.T) {
	body := map[string]interface{}{
		"n":                 float64(3),
		"logprobs":          true,
		"top_logprobs":      2,
		"logit_bias":        map[string]interface{}{"1": -1},
		"metadata":          map[string]interface{}{"trace": "x"},
		"presence_penalty":  0.2,
		"frequency_penalty": 0.2,
		"messages": []interface{}{
			map[string]interface{}{"role": "assistant", "name": "agent", "content": "ok"},
		},
	}
	groq := requestBodyForAttempt(body, modelAttempt{Provider: providerGroq})
	for _, key := range []string{"logprobs", "top_logprobs", "logit_bias", "metadata", "presence_penalty"} {
		if _, ok := groq[key]; ok {
			t.Fatalf("Groq request retained unsupported field %q: %#v", key, groq)
		}
	}
	if got, ok := requestedMaxTokens(groq["n"]); !ok || got != 1 {
		t.Fatalf("Groq n = %#v, want 1", got)
	}
	if _, ok := groq["frequency_penalty"]; !ok {
		t.Fatalf("Groq should retain frequency_penalty because it is supported")
	}
	groqMessage := groq["messages"].([]interface{})[0].(map[string]interface{})
	if _, ok := groqMessage["name"]; ok {
		t.Fatalf("Groq message retained unsupported name: %#v", groqMessage)
	}
	cerebras := requestBodyForAttempt(body, modelAttempt{Provider: providerCerebras})
	for _, key := range []string{"frequency_penalty", "presence_penalty", "logit_bias"} {
		if _, ok := cerebras[key]; ok {
			t.Fatalf("Cerebras request retained unsupported field %q: %#v", key, cerebras)
		}
	}
	if _, ok := cerebras["metadata"]; !ok {
		t.Fatalf("Cerebras should not inherit Groq-only filtering")
	}
	if _, ok := body["logprobs"]; !ok {
		t.Fatalf("source request was mutated")
	}
}

func TestIsProviderCompatibilityError(t *testing.T) {
	if !adapterForProvider(providerGroq).IsCompatibilityError(400, `{"error":"property 'messages.3.assistant.reasoning_content' is unsupported"}`) {
		t.Fatal("expected unsupported property error to advance to another provider")
	}
	if adapterForProvider(providerGroq).IsCompatibilityError(400, `{"error":"invalid API key"}`) {
		t.Fatal("authentication failure is not a schema compatibility error")
	}
	if adapterForProvider(providerGroq).IsCompatibilityError(500, `{"error":"property 'foo' is unsupported"}`) {
		t.Fatal("non-400 upstream failure is not a schema compatibility error")
	}
}

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

func TestInjectLocalAgentGuardrailForMessagesPreservesStructuredSystem(t *testing.T) {
	body := map[string]interface{}{
		"tools": []interface{}{map[string]interface{}{"type": "custom"}},
		"system": []interface{}{
			map[string]interface{}{"type": "text", "text": "keep this Anthropic system prompt"},
		},
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "list files"},
		},
	}
	injectLocalAgentGuardrailForMessages(body)

	system, ok := body["system"].([]interface{})
	if !ok || len(system) != 2 {
		t.Fatalf("structured Anthropic system must remain blocks with guardrail, got %#v", body["system"])
	}
	if !textBlockContains(system[0], localAgentGuardrailMarker) || !textBlockContains(system[1], "keep this Anthropic system prompt") {
		t.Fatalf("structured system content was not preserved: %#v", system)
	}
	messages := body["messages"].([]interface{})
	if len(messages) != 1 || messages[0].(map[string]interface{})["role"] != "user" {
		t.Fatalf("Anthropic messages must not gain system-role entries: %#v", messages)
	}

	injectLocalAgentGuardrailForMessages(body)
	if got := len(body["system"].([]interface{})); got != 2 {
		t.Fatalf("structured system guardrail must be idempotent, got %#v", body["system"])
	}
}

func TestRouteInjectsLocalAgentGuardrailOnBothChatWires(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			var sent map[string]interface{}
			svc := newTestService(t, &http.Client{Transport: &captureTransport{onBody: func(raw string) {
				if err := json.Unmarshal([]byte(raw), &sent); err != nil {
					t.Fatalf("decode upstream body: %v", err)
				}
			}}}, policyConfig{
				DefaultProfile: "coding",
				Profiles:       map[string][]string{"coding": {"tool-model:free"}},
				Aliases:        map[string]string{"default": "coding", "coding": "coding"},
			})
			svc.capabilities = newCapabilityIndex(map[string][]string{"tool-model:free": {"tools"}})

			req := trustedRequest(http.MethodPost, path, `{"tools":[{"type":"function","function":{"name":"list_files"}}],"messages":[{"role":"user","content":"list files"}]}`)
			svc.RouteRequest(httptest.NewRecorder(), req, "test-key")
			if sent == nil {
				t.Fatal("request never reached upstream")
			}
			messages, _ := sent["messages"].([]interface{})
			if path == "/v1/messages" {
				if len(messages) != 1 {
					t.Fatalf("Anthropic messages must not gain system-role entries: %#v", messages)
				}
				if system, _ := sent["system"].(string); !strings.Contains(system, localAgentGuardrailMarker) {
					t.Fatalf("Anthropic system field missing guardrail: %#v", sent)
				}
				for _, raw := range messages {
					if msg, ok := raw.(map[string]interface{}); ok && msg["role"] == "system" {
						t.Fatalf("Anthropic messages must not contain role system: %#v", messages)
					}
				}
			} else {
				if len(messages) < 3 {
					t.Fatalf("guardrail was not injected: %#v", sent["messages"])
				}
				first := messages[0].(map[string]interface{})
				last := messages[len(messages)-1].(map[string]interface{})
				if !messageTextContains(first["content"], localAgentGuardrailMarker) || !messageTextContains(last["content"], localAgentGuardrailMarker) {
					t.Fatalf("missing guardrail bookends: %#v", messages)
				}
			}
			encoded, _ := json.Marshal(sent)
			if !strings.Contains(string(encoded), localAgentGuardrailMarker) {
				t.Fatal("upstream request omitted local runtime marker")
			}
		})
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

func TestClampRequestMaxTokens(t *testing.T) {
	body := map[string]interface{}{"max_tokens": float64(64000)}
	clampRequestMaxTokens(body, 3072)
	if got := body["max_tokens"]; got != 3072 {
		t.Fatalf("max_tokens = %#v, want 3072", got)
	}
	missing := map[string]interface{}{}
	clampRequestMaxTokens(missing, 3072)
	if got := missing["max_tokens"]; got != 3072 {
		t.Fatalf("missing output limit = %#v, want 3072", got)
	}
}
