package cervomodelpolicy

import (
	"encoding/json"
	"fmt"

	configenv "github.com/cervantesh/cervo-config"
)

// RuntimeConfig contains deployment-provided model policy defaults.
type RuntimeConfig struct {
	DefaultModel       string              `config:"CERVO_MODEL_DEFAULT" desc:"Default model for automatic selection in legacy single-profile config"`
	Allowed            []string            `config:"CERVO_MODEL_ALLOWED" sep:"," desc:"Allowed model chain for legacy single-profile config"`
	Mode               string              `config:"CERVO_MODEL_POLICY_MODE" default:"strict" desc:"Policy profile mode for legacy single-profile config"`
	DefaultProfile     string              `config:"CERVO_MODEL_DEFAULT_PROFILE" desc:"Default model profile"`
	Profiles           map[string][]string `config:"-" desc:"Profile to model-chain mapping"`
	Aliases            map[string]string   `config:"-" desc:"Alias to profile mapping"`
	ProfilesJSON       string              `config:"CERVO_MODEL_PROFILES_JSON" desc:"JSON object mapping profiles to model chains"`
	AliasesJSON        string              `config:"CERVO_MODEL_ALIASES_JSON" desc:"JSON object mapping aliases to profiles"`
	ValidationWarnings []ValidationIssue   `config:"-" desc:"Non-fatal model policy validation warnings"`
}

// LoadConfig loads runtime model policy config.
func LoadConfig(loader *configenv.Loader) (RuntimeConfig, error) {
	if loader == nil {
		loader = configenv.New(configenv.Options{})
	}
	var cfg RuntimeConfig
	if err := loader.Decode(&cfg); err != nil {
		return RuntimeConfig{}, err
	}
	if err := decodeRuntimeJSON(cfg.ProfilesJSON, &cfg.Profiles, "CERVO_MODEL_PROFILES_JSON"); err != nil {
		return RuntimeConfig{}, err
	}
	if err := decodeRuntimeJSON(cfg.AliasesJSON, &cfg.Aliases, "CERVO_MODEL_ALIASES_JSON"); err != nil {
		return RuntimeConfig{}, err
	}
	if cfg.DefaultProfile == "" {
		cfg.DefaultProfile = cfg.Mode
	}
	if len(cfg.Profiles) == 0 && cfg.DefaultModel != "" {
		if len(cfg.Allowed) == 0 {
			cfg.Allowed = []string{cfg.DefaultModel}
		}
		if cfg.Mode == "" {
			cfg.Mode = "strict"
		}
		cfg.Profiles = map[string][]string{cfg.Mode: cfg.Allowed}
	}
	if len(cfg.Aliases) == 0 && cfg.DefaultProfile != "" {
		cfg.Aliases = map[string]string{
			"default":          cfg.DefaultProfile,
			cfg.DefaultProfile: cfg.DefaultProfile,
		}
	}
	cfg.ValidationWarnings = ValidateConfig(Config{
		DefaultProfile: cfg.DefaultProfile,
		Profiles:       cfg.Profiles,
		Aliases:        cfg.Aliases,
	})
	return cfg, nil
}

// PolicyFromConfig converts runtime config to a Policy.
func PolicyFromConfig(cfg RuntimeConfig) *Policy {
	if len(cfg.Profiles) > 0 {
		return NewPolicy(Config{
			DefaultProfile: cfg.DefaultProfile,
			Profiles:       cfg.Profiles,
			Aliases:        cfg.Aliases,
		})
	}
	mode := cfg.Mode
	if mode == "" {
		mode = cfg.DefaultProfile
	}
	if mode == "" {
		mode = "strict"
	}
	allowed := cfg.Allowed
	if len(allowed) == 0 && cfg.DefaultModel != "" {
		allowed = []string{cfg.DefaultModel}
	}
	return NewPolicy(Config{
		DefaultProfile: mode,
		Profiles: map[string][]string{
			mode: allowed,
		},
		Aliases: map[string]string{
			"default": mode,
			mode:      mode,
		},
	})
}

func decodeRuntimeJSON[T any](raw string, target *T, field string) error {
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}
