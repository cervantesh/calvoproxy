package router

import "testing"

func TestDefaultAttemptTargetResolver(t *testing.T) {
	resolver := DefaultAttemptTargetResolver{}
	tests := []struct {
		name    string
		attempt modelAttempt
		url     string
		agentic bool
	}{
		{"anthropic", modelAttempt{Provider: providerAnthropic}, anthropicBaseURL + chatCompletionsPath, false},
		{"openai", modelAttempt{Provider: providerOpenAI}, openAIBaseURL + chatCompletionsPath, false},
		{"ollama", modelAttempt{Provider: providerOllama}, ollamaBaseURL + chatCompletionsPath, false},
		{"openrouter default", modelAttempt{Provider: providerOpenRouter}, openRouterChatURL, false},
		{"agentic overrides provider", modelAttempt{Profile: "agent", Provider: providerOpenAI}, geminiCLIChatURL, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolver.Resolve(tc.attempt, chatCompletionsPath)
			if got.URL != tc.url || got.Agentic != tc.agentic {
				t.Fatalf("expected (%q,%v), got (%q,%v)", tc.url, tc.agentic, got.URL, got.Agentic)
			}
		})
	}
}
