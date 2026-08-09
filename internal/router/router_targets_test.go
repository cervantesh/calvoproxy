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
		{"anthropic", modelAttempt{Provider: providerAnthropic}, defaultAnthropicBaseURL + chatCompletionsPath, false},
		{"openai", modelAttempt{Provider: providerOpenAI}, defaultOpenAIBaseURL + chatCompletionsPath, false},
		{"ollama", modelAttempt{Provider: providerOllama}, defaultOllamaBaseURL + chatCompletionsPath, false},
		{"cerebras", modelAttempt{Provider: providerCerebras}, defaultCerebrasChatURL, false},
		{"groq", modelAttempt{Provider: providerGroq}, defaultGroqChatURL, false},
		{"openrouter default", modelAttempt{Provider: providerOpenRouter}, defaultOpenRouterChatURL, false},
		// With no PROXY_AGENTIC_URL configured, an agent profile falls through to
		// normal provider routing (OpenRouter default) instead of a sidecar.
		{"agent falls through when unconfigured", modelAttempt{Profile: "agent", Provider: providerOpenRouter}, defaultOpenRouterChatURL, false},
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

func TestAgenticTargetUsesConfiguredURL(t *testing.T) {
	t.Setenv("PROXY_AGENTIC_URL", "http://agent.local:9000/v1/chat/completions")
	got := DefaultAttemptTargetResolver{}.Resolve(modelAttempt{Profile: "agent", Provider: providerOpenRouter}, chatCompletionsPath)
	if got.URL != "http://agent.local:9000/v1/chat/completions" || !got.Agentic {
		t.Fatalf("expected configured agentic URL, got (%q,%v)", got.URL, got.Agentic)
	}
}

func TestTargetURLEnvOverride(t *testing.T) {
	t.Setenv("PROXY_OPENROUTER_URL", "http://mock:1234/v1/chat/completions")
	got := DefaultAttemptTargetResolver{}.Resolve(modelAttempt{Provider: providerOpenRouter}, chatCompletionsPath)
	if got.URL != "http://mock:1234/v1/chat/completions" {
		t.Fatalf("expected env override, got %q", got.URL)
	}
}
