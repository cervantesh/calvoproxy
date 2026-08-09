package router

import (
	"strconv"
	"strings"
)

// hasRequestTools reports whether the request actually asks for tool calling.
//
// Only a NON-EMPTY ARRAY counts. Previously any non-nil value did, so `"tools": {}`
// or `"tools": []` — both of which ask for nothing — forced the tools capability
// requirement, shrinking the eligible chain to tool-capable models for no reason
// and, on a vision request, pushing it onto the narrower vision+tools rescue path.
func hasRequestTools(reqBody map[string]interface{}) bool {
	if tools, ok := reqBody["tools"].([]interface{}); ok && len(tools) > 0 {
		return true
	}
	if functions, ok := reqBody["functions"].([]interface{}); ok && len(functions) > 0 {
		return true
	}
	return false
}

// localAgentGuardrailMarker is the unique prefix of the system message we inject
// on tool-calling requests. Free models (OpenRouter :free) often invent
// "I'm in a sandbox and cannot operate on your computer" even after successful
// local tool calls — OpenCode clients hit this constantly. The marker keeps
// injection idempotent across retries and multi-turn history rewrites.
const localAgentGuardrailMarker = "CALVOPROXY_LOCAL_RUNTIME"

// localAgentGuardrail is short on purpose: free models ignore long policies, and
// every token here is paid for (even at $0) by context pressure on agent turns.
// Placed BOTH first and last in the message list — free models are recency-biased
// and often only "hear" the final system/user turn.
const localAgentGuardrail = localAgentGuardrailMarker + `: HARD CONSTRAINT — LOCAL MACHINE. OpenCode is running ON the user's real Windows PC (not remote, not Docker, not a VM sandbox for you). Working directory C:\dev\... and every tool result are THE USER's files on THEIR disk. Speak as a local agent on their computer. NEVER invent a separate "your" vs "their" world. FORBIDDEN (ES/EN, any spelling): "sandbox", "mi sandbox", "mi entorno", "mi entorno actual", "esto es mi entorno", "no tu máquina", "not your machine", "my environment", "my directory not yours", "I cannot access your computer", "no puedo operar sobre tu computadora", "ejecutalo vos porque yo no tengo acceso". CORRECT framing: "en el directorio actual (C:\...)", "en tu máquina", "archivos del proyecto". After a successful tool call you already proved local access — never walk it back. On tool failure: report the real error only.`

// injectLocalAgentGuardrail pins free models to local-runtime reality on agent
// turns. Only when tools are present. Idempotent: strips prior copies of the
// marker, then re-attaches one at the front and one at the end so recency-biased
// free models still see it on every turn (including multi-turn agent loops).
func injectLocalAgentGuardrail(reqBody map[string]interface{}) {
	if reqBody == nil || !hasRequestTools(reqBody) {
		return
	}
	// Anthropic-shaped bodies put the system prompt in a top-level field.
	if sys, ok := reqBody["system"].(string); ok && sys != "" && !strings.Contains(sys, localAgentGuardrailMarker) {
		reqBody["system"] = localAgentGuardrail + "\n\n" + sys
	}
	messages, ok := reqBody["messages"].([]interface{})
	if !ok {
		return
	}
	cleaned := make([]interface{}, 0, len(messages)+2)
	for _, raw := range messages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			cleaned = append(cleaned, raw)
			continue
		}
		role, _ := msg["role"].(string)
		if strings.EqualFold(role, "system") && messageTextContains(msg["content"], localAgentGuardrailMarker) {
			continue // drop prior injects; we re-add fresh bookends
		}
		cleaned = append(cleaned, msg)
	}
	guard := map[string]interface{}{
		"role":    "system",
		"content": localAgentGuardrail,
	}
	out := make([]interface{}, 0, len(cleaned)+2)
	out = append(out, guard)
	out = append(out, cleaned...)
	out = append(out, guard)
	// Also tag the last user turn — free models weight the latest user text
	// more than distant system prompts.
	for i := len(out) - 1; i >= 0; i-- {
		msg, ok := out[i].(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if !strings.EqualFold(role, "user") {
			continue
		}
		if messageTextContains(msg["content"], localAgentGuardrailMarker) {
			break
		}
		out[i] = map[string]interface{}{
			"role":    "user",
			"content": appendUserRuntimeTag(msg["content"]),
		}
		break
	}
	reqBody["messages"] = out
}

const userRuntimeTag = "\n\n[" + localAgentGuardrailMarker + ": REAL local PC. Tool output = user's machine. Never say sandbox / mi entorno / my environment / no tu máquina.]"

func appendUserRuntimeTag(content interface{}) interface{} {
	switch typed := content.(type) {
	case string:
		return typed + userRuntimeTag
	case []interface{}:
		// Multimodal content parts: append a trailing text part.
		parts := make([]interface{}, 0, len(typed)+1)
		parts = append(parts, typed...)
		parts = append(parts, map[string]interface{}{
			"type": "text",
			"text": strings.TrimSpace(userRuntimeTag),
		})
		return parts
	default:
		return content
	}
}

func messageTextContains(content interface{}, needle string) bool {
	switch typed := content.(type) {
	case string:
		return strings.Contains(typed, needle)
	case []interface{}:
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]interface{})
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok && strings.Contains(text, needle) {
				return true
			}
		}
	}
	return false
}

type policyRequestFacts struct {
	Metadata        map[string]string
	RequestedLimits RequestedLimits
}

func requestPolicyFacts(reqBody map[string]interface{}, profile string, requestedModel string, stream bool, hasTools bool, hasImages bool, bodyBytes int64) policyRequestFacts {
	metadata := map[string]string{}
	requested := RequestedLimits{
		BodyBytes: bodyBytes,
		Stream:    stream,
		Tools:     hasTools,
		Images:    hasImages,
	}
	if raw, ok := reqBody["max_tokens"]; ok {
		if maxTokens, ok := requestedMaxTokens(raw); ok {
			requested.MaxTokens = maxTokens
		}
	}
	if strings.TrimSpace(profile) != "" {
		metadata["profile"] = strings.TrimSpace(profile)
	}
	if strings.TrimSpace(requestedModel) != "" {
		metadata["requested_model"] = strings.TrimSpace(requestedModel)
	}
	return policyRequestFacts{
		Metadata:        metadata,
		RequestedLimits: requested,
	}
}

func requestedMaxTokens(raw any) (int, bool) {
	switch typed := raw.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case string:
		value := strings.TrimSpace(typed)
		if value == "" {
			return 0, false
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}
