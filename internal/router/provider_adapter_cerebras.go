package router

type cerebrasAdapter struct{}

func (cerebrasAdapter) NormalizeRequest(body map[string]interface{}) map[string]interface{} {
	copy := cloneRequestBody(body)
	for _, key := range []string{"frequency_penalty", "presence_penalty", "logit_bias"} {
		delete(copy, key)
	}
	if _, exists := body["messages"]; exists {
		copy["messages"] = normalizeMessages(body["messages"], func(message map[string]interface{}) {
			if reasoning, ok := message["reasoning_content"]; ok {
				if _, alreadyPresent := message["reasoning"]; !alreadyPresent {
					message["reasoning"] = reasoning
				}
			}
			delete(message, "reasoning_content")
		})
	}
	return copy
}

func (cerebrasAdapter) IsCompatibilityError(status int, body string) bool {
	return isUnsupportedSchemaError(status, body)
}
