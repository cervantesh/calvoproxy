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
