package router

type groqAdapter struct{}

func (groqAdapter) NormalizeRequest(body map[string]interface{}) map[string]interface{} {
	copy := cloneRequestBody(body)
	for _, key := range []string{"logprobs", "top_logprobs", "logit_bias", "metadata", "presence_penalty"} {
		delete(copy, key)
	}
	if n, ok := requestedMaxTokens(copy["n"]); ok && n != 1 {
		copy["n"] = 1
	}
	if _, exists := body["messages"]; exists {
		copy["messages"] = normalizeMessages(body["messages"], func(message map[string]interface{}) {
			delete(message, "reasoning_content")
			delete(message, "name")
		})
	}
	return copy
}

func (groqAdapter) IsCompatibilityError(status int, body string) bool {
	return isUnsupportedSchemaError(status, body)
}

func normalizeMessages(raw interface{}, normalize func(map[string]interface{})) interface{} {
	messages, ok := raw.([]interface{})
	if !ok {
		return raw
	}
	cloned := make([]interface{}, len(messages))
	for i, rawMessage := range messages {
		message, ok := rawMessage.(map[string]interface{})
		if !ok {
			cloned[i] = rawMessage
			continue
		}
		messageCopy := make(map[string]interface{}, len(message))
		for key, value := range message {
			messageCopy[key] = value
		}
		normalize(messageCopy)
		cloned[i] = messageCopy
	}
	return cloned
}
