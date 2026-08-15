package router

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"

	cervorules "github.com/cervantesh/cervo-rules/v3/core"
)

type providerID = cervorules.Executor

// providerModelTarget is the smallest useful routing unit: one provider and
// one model that is valid at that provider. Keeping the pair explicit avoids
// the old model × provider Cartesian product, which generated invalid routes
// and risked sending a credential to the wrong upstream.
type providerModelTarget struct {
	Provider providerID `json:"Provider"`
	Model    string     `json:"Model"`
}

type providerProfiles map[string][]providerModelTarget

type providerProfilesSchema struct {
	ProviderProfiles providerProfiles `json:"ProviderProfiles"`
}

var supportedDirectProviders = map[providerID]struct{}{
	providerOpenRouter: {},
	providerCerebras:   {},
	providerGroq:       {},
	providerOllama:     {},
}

func parseProviderProfiles(data []byte) (providerProfiles, bool) {
	var schema providerProfilesSchema
	if err := json.Unmarshal(data, &schema); err != nil || schema.ProviderProfiles == nil {
		return nil, false
	}
	out := make(providerProfiles, len(schema.ProviderProfiles))
	for profile, targets := range schema.ProviderProfiles {
		profile = strings.TrimSpace(profile)
		if profile == "" {
			continue
		}
		seen := map[string]struct{}{}
		for _, target := range targets {
			target.Provider = providerID(strings.ToLower(strings.TrimSpace(string(target.Provider))))
			target.Model = strings.TrimSpace(target.Model)
			if _, ok := supportedDirectProviders[target.Provider]; !ok || target.Model == "" {
				slog.Warn("[CalvoProxy] ignoring invalid provider profile target",
					slog.String("profile", profile), slog.String("provider", string(target.Provider)), slog.String("model", target.Model))
				continue
			}
			key := string(target.Provider) + "\x00" + target.Model
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			out[profile] = append(out[profile], target)
		}
	}
	return out, true
}

// loadProviderProfiles reads the explicit provider+model chains from the same
// hot-reloadable model-policy.json as the legacy model-only profiles. An
// external file that omits ProviderProfiles deliberately stays in legacy mode.
func loadProviderProfiles() providerProfiles {
	for _, path := range modelPolicyFileCandidates() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		profiles, _ := parseProviderProfiles(data)
		return profiles
	}
	profiles, _ := parseProviderProfiles(defaultModelConfigJSON)
	return profiles
}

func cloneProviderProfiles(in providerProfiles) providerProfiles {
	if in == nil {
		return nil
	}
	out := make(providerProfiles, len(in))
	for profile, targets := range in {
		out[profile] = append([]providerModelTarget(nil), targets...)
	}
	return out
}

type ambientProviderCredentialsKey struct{}

// WithAmbientProviderCredentials records the HTTP-layer decision that locally
// configured provider keys may be used for this request. The command package
// sets false on a public bind unless the operator explicitly opts in.
func WithAmbientProviderCredentials(ctx context.Context, allowed bool) context.Context {
	return context.WithValue(ctx, ambientProviderCredentialsKey{}, allowed)
}

func ambientProviderCredentialsAllowed(ctx context.Context) bool {
	allowed, _ := ctx.Value(ambientProviderCredentialsKey{}).(bool)
	return allowed
}

func providerCredential(ctx context.Context, attempt modelAttempt, openRouterRequestKey string) (string, bool) {
	return (*RouterService)(nil).providerCredential(ctx, attempt, openRouterRequestKey)
}

func (s *RouterService) providerCredential(ctx context.Context, attempt modelAttempt, openRouterRequestKey string) (string, bool) {
	var key string
	switch attempt.Provider {
	case providerCerebras:
		if ambientProviderCredentialsAllowed(ctx) {
			key = envValue("CEREBRAS_API_KEY")
			if key == "" {
				key = s.resolvedAmbientProviderCredential(attempt.Provider)
			}
		}
	case providerGroq:
		if ambientProviderCredentialsAllowed(ctx) {
			key = envValue("GROQ_API_KEY")
			if key == "" {
				key = s.resolvedAmbientProviderCredential(attempt.Provider)
			}
		}
	case providerOllama, providerLocal:
		// Ollama is a local, keyless resource. Do not route a public request to
		// the host machine merely because it supplied an unrelated API key.
		// Operators may opt in on a public bind through the same ambient-resource
		// gate that protects locally configured provider credentials.
		if ambientProviderCredentialsAllowed(ctx) {
			key = "local"
		}
	default:
		// Allowlist, not fallback: the caller's OpenRouter request key may only
		// ever travel to OpenRouter. An attempt with no explicit provider
		// inherits the operator-configured default executor, so it qualifies
		// only while that default is still OpenRouter. Every other provider
		// (openai, anthropic, ...) must supply its own configured credential;
		// without one it returns ("", false) and filterConfiguredAttempts drops
		// the attempt, instead of sending an sk-or-... key to an unrelated
		// third-party upstream. Direct-provider keys likewise never enter here.
		if s.credentialProvider(attempt) == providerOpenRouter {
			key = strings.TrimSpace(openRouterRequestKey)
		}
	}
	return key, key != ""
}

// credentialProvider resolves the provider that will actually receive this
// attempt. It mirrors normalizedProvider (an empty Provider inherits the
// operator's default executor) but tolerates a nil receiver, because the
// package-level providerCredential helper calls through (*RouterService)(nil).
func (s *RouterService) credentialProvider(attempt modelAttempt) providerID {
	if attempt.Provider != "" {
		return attempt.Provider
	}
	if s == nil {
		return providerOpenRouter
	}
	return s.defaultPolicyProvider()
}

func (s *RouterService) resolvedAmbientProviderCredential(provider providerID) string {
	if s == nil || s.AmbientProviderCredential == nil {
		return ""
	}
	secret, ok := s.AmbientProviderCredential(string(provider))
	if !ok || len(secret) == 0 {
		clear(secret)
		return ""
	}
	key := strings.TrimSpace(string(secret))
	clear(secret)
	return key
}

func (s *RouterService) filterConfiguredAttempts(ctx context.Context, attempts []modelAttempt, openRouterRequestKey, opPath string) []modelAttempt {
	out := make([]modelAttempt, 0, len(attempts))
	for _, attempt := range attempts {
		// Cerebras and Groq expose OpenAI chat completions, not the Anthropic
		// Messages wire format CalvoProxy also accepts.
		if opPath == messagesPath && (attempt.Provider == providerCerebras || attempt.Provider == providerGroq) {
			continue
		}
		if _, ok := s.providerCredential(ctx, attempt, openRouterRequestKey); !ok {
			continue
		}
		out = append(out, attempt)
	}
	return out
}
