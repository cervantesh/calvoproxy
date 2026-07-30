package router

import (
	"context"
	"encoding/json"
)

type SideEffectResponseTransformer struct {
	Extractor SideEffectExtractor
}

func NewSideEffectResponseTransformer(extractor SideEffectExtractor) SideEffectResponseTransformer {
	return SideEffectResponseTransformer{Extractor: extractor}
}

func (t SideEffectResponseTransformer) Transform(ctx context.Context, body []byte) []byte {
	if t.Extractor == nil {
		return body
	}

	var chatResp map[string]any
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return body
	}

	message, ok := firstChatMessage(chatResp)
	if !ok {
		return body
	}

	content, _ := message["content"].(string)
	if metadata := t.Extractor.Extract(ctx, content); metadata != nil {
		message["metadata"] = metadata
		if transformed, err := json.Marshal(chatResp); err == nil {
			return transformed
		}
	}
	return body
}

func firstChatMessage(chatResp map[string]any) (map[string]any, bool) {
	choices, ok := chatResp["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, false
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil, false
	}
	message, ok := choice["message"].(map[string]any)
	return message, ok
}

func (s *RouterService) transformResponse(ctx context.Context, body []byte) []byte {
	transformer := s.Transformer
	if transformer == nil {
		transformer = NewSideEffectResponseTransformer(s.SideEffects)
	}
	return transformer.Transform(ctx, body)
}
