package cervomodelpolicy

import "strings"

// Config maps profile names and aliases to ordered model chains.
//
// Profile names and aliases are normalized to lowercase by NewPolicy. Model
// names are trimmed but otherwise preserved, so provider-qualified values such
// as "openai/gpt-4.1" pass through unchanged.
type Config struct {
	DefaultProfile string
	Profiles       map[string][]string
	Aliases        map[string]string
}

// Request asks Policy to resolve a profile and requested model into an ordered
// attempt chain.
type Request struct {
	Profile        string
	RequestedModel string
}

// Decision is the resolved model profile and fallback chain returned by Select.
type Decision struct {
	Profile        string
	ModelChain     []string
	FallbackModels []string
	Reason         string
}

// Policy resolves profiles, aliases, and fallback model chains.
type Policy struct {
	cfg Config
}

// NewPolicy creates a normalized model policy.
func NewPolicy(cfg Config) *Policy {
	return &Policy{cfg: NormalizeConfig(cfg)}
}

// Config returns a defensive copy of the normalized policy config.
func (p *Policy) Config() Config {
	if p == nil {
		p = NewPolicy(Config{})
	}
	return cloneConfig(p.cfg)
}

// NormalizeConfig returns a cleaned copy of a model config.
func NormalizeConfig(cfg Config) Config {
	if strings.TrimSpace(cfg.DefaultProfile) == "" {
		cfg.DefaultProfile = "default"
	}
	cfg.DefaultProfile = strings.ToLower(strings.TrimSpace(cfg.DefaultProfile))

	profiles := map[string][]string{}
	for profile, chain := range cfg.Profiles {
		trimmedProfile := strings.ToLower(strings.TrimSpace(profile))
		if trimmedProfile == "" {
			continue
		}
		cleanChain := make([]string, 0, len(chain))
		for _, model := range chain {
			if trimmed := strings.TrimSpace(model); trimmed != "" {
				cleanChain = append(cleanChain, trimmed)
			}
		}
		if len(cleanChain) > 0 {
			profiles[trimmedProfile] = cleanChain
		}
	}
	cfg.Profiles = profiles
	if len(cfg.Profiles) == 0 {
		defaults := DefaultConfig()
		cfg.Profiles = cloneProfiles(defaults.Profiles)
		if _, ok := cfg.Profiles[cfg.DefaultProfile]; !ok {
			cfg.DefaultProfile = defaults.DefaultProfile
		}
	}

	aliases := map[string]string{}
	for alias, profile := range cfg.Aliases {
		trimmedAlias := strings.ToLower(strings.TrimSpace(alias))
		trimmedProfile := strings.ToLower(strings.TrimSpace(profile))
		if trimmedAlias == "" || trimmedProfile == "" {
			continue
		}
		if _, ok := cfg.Profiles[trimmedProfile]; ok {
			aliases[trimmedAlias] = trimmedProfile
		}
	}
	for profile := range cfg.Profiles {
		aliases[profile] = profile
	}
	cfg.Aliases = aliases

	if _, ok := cfg.Profiles[cfg.DefaultProfile]; !ok {
		for profile := range cfg.Profiles {
			cfg.DefaultProfile = profile
			break
		}
	}
	cfg.Aliases["default"] = cfg.DefaultProfile
	return cfg
}

// DefaultConfig returns a minimal deterministic model policy.
func DefaultConfig() Config {
	return Config{
		DefaultProfile: "default",
		Profiles: map[string][]string{
			"default": {"auto"},
		},
		Aliases: map[string]string{
			"default": "default",
		},
	}
}

// Select resolves a profile and returns the model attempt chain.
func (p *Policy) Select(req Request) Decision {
	if p == nil {
		p = NewPolicy(Config{})
	}
	profile := strings.ToLower(strings.TrimSpace(req.Profile))
	if _, ok := p.cfg.Profiles[profile]; !ok {
		profile = p.cfg.DefaultProfile
	}
	selected := p.cfg.Profiles[profile]
	if len(selected) == 0 {
		selected = p.cfg.Profiles[p.cfg.DefaultProfile]
		profile = p.cfg.DefaultProfile
	}

	trimmedModel := strings.TrimSpace(req.RequestedModel)
	if trimmedModel == "" || strings.EqualFold(trimmedModel, "auto") {
		chain := append([]string(nil), selected...)
		return Decision{
			Profile:        profile,
			ModelChain:     chain,
			FallbackModels: fallbackModels(chain),
			Reason:         "model chain selected from profile " + profile,
		}
	}

	seen := map[string]struct{}{trimmedModel: {}}
	chain := []string{trimmedModel}
	for _, model := range selected {
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		chain = append(chain, model)
	}
	return Decision{
		Profile:        profile,
		ModelChain:     chain,
		FallbackModels: fallbackModels(chain),
		Reason:         "requested model prepended to profile " + profile,
	}
}

// ResolveModelAlias treats a bare profile alias in the model field as profile selection.
func (p *Policy) ResolveModelAlias(profile string, requestedModel string) (string, string) {
	if p == nil {
		p = NewPolicy(Config{})
	}
	if reqModelStr := strings.TrimSpace(requestedModel); reqModelStr != "" {
		if alias, ok := p.ResolveProfileAlias(reqModelStr); ok {
			return alias, "auto"
		}
	}
	return profile, requestedModel
}

// ResolveProfileAlias resolves a profile alias.
func (p *Policy) ResolveProfileAlias(raw string) (string, bool) {
	if p == nil {
		p = NewPolicy(Config{})
	}
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return "", false
	}
	category, ok := p.cfg.Aliases[trimmed]
	if !ok {
		return "", false
	}
	return category, true
}

func fallbackModels(chain []string) []string {
	if len(chain) <= 1 {
		return nil
	}
	return append([]string(nil), chain[1:]...)
}

func cloneProfiles(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for profile, chain := range in {
		out[profile] = append([]string(nil), chain...)
	}
	return out
}

func cloneAliases(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for alias, profile := range in {
		out[alias] = profile
	}
	return out
}

func cloneConfig(cfg Config) Config {
	return Config{
		DefaultProfile: cfg.DefaultProfile,
		Profiles:       cloneProfiles(cfg.Profiles),
		Aliases:        cloneAliases(cfg.Aliases),
	}
}
