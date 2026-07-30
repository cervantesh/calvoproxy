package router

const (
	chatCompletionsPath = "/v1/chat/completions"
	anthropicBaseURL    = "https://api.anthropic.com"
	openAIBaseURL       = "https://api.openai.com"
	ollamaBaseURL       = "http://ollama:11434"
	openRouterChatURL   = "https://openrouter.ai/api/v1/chat/completions"
	geminiCLIChatURL    = "http://gemini-cli-api:8091/v1/chat/completions"
)

type DefaultAttemptTargetResolver struct{}

func (DefaultAttemptTargetResolver) Resolve(attempt modelAttempt, path string) AttemptTarget {
	if isAgenticProfile(attempt.Profile) {
		return AttemptTarget{URL: geminiCLIChatURL, Agentic: true}
	}
	switch attempt.Provider {
	case providerAnthropic:
		return AttemptTarget{URL: anthropicBaseURL + path}
	case providerOpenAI:
		return AttemptTarget{URL: openAIBaseURL + path}
	case providerOllama, providerLocal:
		return AttemptTarget{URL: ollamaBaseURL + path}
	default:
		return AttemptTarget{URL: openRouterChatURL}
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
