package router

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSideEffectResponseTransformerInjectsMetadata(t *testing.T) {
	transformer := NewSideEffectResponseTransformer(staticSideEffects{metadata: map[string]any{"source": "test"}})
	body := []byte(`{"choices":[{"message":{"content":"changed files"}}]}`)

	transformed := transformer.Transform(context.Background(), body)

	var response map[string]any
	if err := json.Unmarshal(transformed, &response); err != nil {
		t.Fatalf("invalid transformed response: %v", err)
	}
	message, ok := firstChatMessage(response)
	if !ok {
		t.Fatal("expected chat message")
	}
	metadata := message["metadata"].(map[string]any)
	if metadata["source"] != "test" {
		t.Fatalf("expected injected metadata, got %+v", metadata)
	}
}

func TestSideEffectResponseTransformerLeavesNonChatBodyUntouched(t *testing.T) {
	transformer := NewSideEffectResponseTransformer(staticSideEffects{metadata: map[string]any{"source": "test"}})
	body := []byte(`{"embedding":[1,2,3]}`)

	transformed := transformer.Transform(context.Background(), body)

	if string(transformed) != string(body) {
		t.Fatalf("expected unchanged body, got %s", transformed)
	}
}
