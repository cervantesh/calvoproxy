package router

const (
	chatCompletionsPath      = "/v1/chat/completions"
	defaultAnthropicBaseURL  = "https://api.anthropic.com"
	defaultOpenAIBaseURL     = "https://api.openai.com"
	defaultOllamaBaseURL     = "http://ollama:11434"
	defaultOpenRouterChatURL = "https://openrouter.ai/api/v1/chat/completions"
)

// targetURL returns the env override for an upstream base URL, or the default.
// Lets an operator point CalvoProxy at a real Anthropic/OpenAI/Ollama endpoint
// (or a local mock) instead of the monorepo's docker-service defaults.
func targetURL(env, def string) string {
	if v := envValue(env); v != "" {
		return v
	}
	return def
}

type DefaultAttemptTargetResolver struct{}

func (DefaultAttemptTargetResolver) Resolve(attempt modelAttempt, path string) AttemptTarget {
	// Agentic profiles (agent/agentic/plan) route to a dedicated agent endpoint
	// ONLY when one is configured via PROXY_AGENTIC_URL. Otherwise they fall
	// through to normal provider routing, so the `agent` profile works
	// standalone (→ OpenRouter) instead of failing to reach an absent sidecar.
	if isAgenticProfile(attempt.Profile) {
		if agentic := envValue("PROXY_AGENTIC_URL"); agentic != "" {
			return AttemptTarget{URL: agentic, Agentic: true}
		}
	}
	switch attempt.Provider {
	case providerAnthropic:
		return AttemptTarget{URL: targetURL("PROXY_ANTHROPIC_URL", defaultAnthropicBaseURL) + path}
	case providerOpenAI:
		return AttemptTarget{URL: targetURL("PROXY_OPENAI_URL", defaultOpenAIBaseURL) + path}
	case providerOllama, providerLocal:
		return AttemptTarget{URL: targetURL("PROXY_OLLAMA_URL", defaultOllamaBaseURL) + path}
	default:
		return AttemptTarget{URL: targetURL("PROXY_OPENROUTER_URL", defaultOpenRouterChatURL)}
	}
}

func (s *RouterService) resolveAttemptTarget(attempt modelAttempt, path string) AttemptTarget {
	resolver := s.TargetResolver
	if resolver == nil {
		resolver = DefaultAttemptTargetResolver{}
	}
	return resolver.Resolve(attempt, path)
}

func isAgenticProfile(profile string) bool {
	switch profile {
	case "agent", "agentic", "plan":
		return true
	default:
		return false
	}
}
