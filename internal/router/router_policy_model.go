package router

import (
	_ "embed"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	cervomodelpolicy "github.com/cervantesh/cervo-model-policy"
)

// The embedded copy is a last-resort default so the binary always boots with a
// valid model policy. The live, editable source is an external model-policy.json
// (see loadModelConfigFile) — edit it and restart, no rebuild needed.
//
//go:embed config/model-policy.default.json
var defaultModelConfigJSON []byte

// modelPolicyFileCandidates lists, in precedence order, where an external
// model policy file may live: an explicit PROXY_MODEL_POLICY_FILE path, then
// model-policy.json next to the executable, then in the working directory.
func modelPolicyFileCandidates() []string {
	paths := []string{}
	if p := envValue("PROXY_MODEL_POLICY_FILE"); p != "" {
		paths = append(paths, p)
	}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "model-policy.json"))
	}
	paths = append(paths, "model-policy.json")
	return paths
}

// loadModelConfigFile reads the external model policy file if one exists,
// letting operators change the free-model chains without recompiling. Returns
// ok=false (silently) when no file is present, so the embedded default is used.
func loadModelConfigFile() (cervomodelpolicy.Config, string, bool) {
	var cfg cervomodelpolicy.Config
	for _, path := range modelPolicyFileCandidates() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			slog.Warn("[CalvoProxy] invalid external model policy file; using embedded default",
				slog.String("path", path), slog.Any("error", err))
			return cervomodelpolicy.Config{}, path, false
		}
		return cervomodelpolicy.NormalizeConfig(cfg), path, true
	}
	return cfg, "", false
}

type modelPolicyRuntime struct {
	Config   policyConfig
	Warnings []cervomodelpolicy.ValidationIssue
	Strict   bool
}

func (s *RouterService) resolveProfileAlias(raw string) (string, bool) {
	return s.activeModelPolicy().ResolveProfileAlias(raw)
}

func policyConfigFromModelConfig(cfg cervomodelpolicy.Config) policyConfig {
	cfg = cervomodelpolicy.NormalizeConfig(cfg)
	return policyConfig{
		DefaultProfile: cfg.DefaultProfile,
		Profiles:       cfg.Profiles,
		Aliases:        cfg.Aliases,
	}
}

func defaultModelConfig() cervomodelpolicy.Config {
	var cfg cervomodelpolicy.Config
	if err := json.Unmarshal(defaultModelConfigJSON, &cfg); err != nil {
		slog.Error("[CalvoProxy] embedded default model policy is invalid", slog.Any("error", err))
		return cervomodelpolicy.DefaultConfig()
	}
	return cervomodelpolicy.NormalizeConfig(cfg)
}

func loadModelConfigFromEnv() cervomodelpolicy.Config {
	return loadModelPolicyRuntime().Config
}

func loadModelPolicyRuntime() modelPolicyRuntime {
	policy := defaultModelConfig()
	// External file overrides the embedded default (env still wins over both).
	if fileCfg, path, ok := loadModelConfigFile(); ok {
		if len(fileCfg.Profiles) > 0 {
			policy.Profiles = fileCfg.Profiles
		}
		if len(fileCfg.Aliases) > 0 {
			policy.Aliases = fileCfg.Aliases
		}
		if fileCfg.DefaultProfile != "" {
			policy.DefaultProfile = fileCfg.DefaultProfile
		}
		slog.Info("[CalvoProxy] loaded model policy from file", slog.String("path", path))
	}
	strict := envBool("CERVO_MODEL_POLICY_STRICT", false)
	warnings := []cervomodelpolicy.ValidationIssue{}
	if hasAnyEnv(
		"CERVO_MODEL_DEFAULT_PROFILE",
		"CERVO_MODEL_PROFILES_JSON",
		"CERVO_MODEL_ALIASES_JSON",
		"CERVO_MODEL_DEFAULT",
		"CERVO_MODEL_ALLOWED",
		"CERVO_MODEL_POLICY_MODE",
	) {
		runtimeConfig, err := cervomodelpolicy.LoadConfig(nil)
		if err != nil {
			slog.Warn("[CalvoProxy] invalid CERVO_MODEL policy config", slog.Any("error", err))
			warnings = append(warnings, modelPolicyRuntimeWarning("CERVO_MODEL", err.Error()))
		} else {
			if len(runtimeConfig.Profiles) > 0 {
				policy.Profiles = runtimeConfig.Profiles
			}
			if len(runtimeConfig.Aliases) > 0 {
				policy.Aliases = runtimeConfig.Aliases
			}
			if shouldUseRuntimeDefaultProfile() && runtimeConfig.DefaultProfile != "" {
				policy.DefaultProfile = runtimeConfig.DefaultProfile
			}
		}
	}
	if raw := envValue("PROXY_PROVIDER_PROFILES_JSON"); raw != "" {
		candidate := map[string][]string{}
		if err := json.Unmarshal([]byte(raw), &candidate); err != nil {
			slog.Warn("[CalvoProxy] invalid PROXY_PROVIDER_PROFILES_JSON", slog.Any("error", err))
			warnings = append(warnings, modelPolicyRuntimeWarning("PROXY_PROVIDER_PROFILES_JSON", err.Error()))
		} else {
			policy.Profiles = candidate
		}
	}
	if raw := envValue("PROXY_PROVIDER_ALIASES_JSON"); raw != "" {
		candidate := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &candidate); err != nil {
			slog.Warn("[CalvoProxy] invalid PROXY_PROVIDER_ALIASES_JSON", slog.Any("error", err))
			warnings = append(warnings, modelPolicyRuntimeWarning("PROXY_PROVIDER_ALIASES_JSON", err.Error()))
		} else {
			policy.Aliases = candidate
		}
	}
	if raw := canonicalEnvValue("PROXY_DEFAULT_PROFILE"); raw != "" {
		policy.DefaultProfile = raw
	}
	warnings = append(warnings, cervomodelpolicy.ValidateConfig(policy)...)
	for _, issue := range warnings {
		slog.Warn("[CalvoProxy] model policy config warning",
			slog.String("code", string(issue.Code)),
			slog.String("field", issue.Field),
			slog.String("message", issue.Message),
		)
	}
	return modelPolicyRuntime{
		Config:   cervomodelpolicy.NormalizeConfig(policy),
		Warnings: warnings,
		Strict:   strict,
	}
}

func modelPolicyRuntimeWarning(field string, message string) cervomodelpolicy.ValidationIssue {
	return cervomodelpolicy.ValidationIssue{
		Code:    cervomodelpolicy.ValidationCode("runtime_config_invalid"),
		Field:   field,
		Message: message,
	}
}

func hasAnyEnv(keys ...string) bool {
	for _, key := range keys {
		if envValue(key) != "" {
			return true
		}
	}
	return false
}

func shouldUseRuntimeDefaultProfile() bool {
	return hasAnyEnv("CERVO_MODEL_DEFAULT_PROFILE", "CERVO_MODEL_DEFAULT", "CERVO_MODEL_POLICY_MODE")
}

func (s *RouterService) ModelPolicyHealth() ModelPolicyHealth {
	cfg := s.getPolicy()
	return ModelPolicyHealth{
		DefaultProfile:     cfg.DefaultProfile,
		Profiles:           profileNames(cfg.Profiles),
		Aliases:            aliasNames(cfg.Aliases),
		Strict:             s.modelStrict,
		ValidationWarnings: append([]cervomodelpolicy.ValidationIssue(nil), s.modelWarnings...),
	}
}

func aliasNames(aliases map[string]string) []string {
	names := make([]string, 0, len(aliases))
	for name := range aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
