package router

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/cervantesh/calvoproxy/internal/router/policyrules"
	cervomodelpolicy "github.com/cervantesh/cervo-model-policy"
	cervorules "github.com/cervantesh/cervo-rules/v3/core"
	cervoruntime "github.com/cervantesh/cervo-rules/v3/runtime"
)

type policyConfig = cervomodelpolicy.Config

func loadPolicyConfig() policyConfig {
	return policyConfigFromModelConfig(loadModelConfigFromEnv())
}

func loadRuleConfig(defaultTimeout time.Duration, failureThreshold int, cooldown time.Duration) ruleRuntimeConfig {
	cfg := baseRuleConfig(defaultTimeout, failureThreshold, cooldown)
	cfg.OperationTargets = loadRouteOverridesFromEnv()
	cfg.ExecutorFallbacks = loadExecutorFallbacksFromEnv()
	return cfg
}

func baseRuleConfig(defaultTimeout time.Duration, failureThreshold int, cooldown time.Duration) ruleRuntimeConfig {
	retry := loadRetryPolicyFromEnv()
	breaker := BreakerPolicy{
		FailureThreshold: failureThreshold,
		Cooldown:         cooldown,
		Eligible:         true,
	}
	limits := loadLimitsFromEnv()
	cfg := ruleRuntimeConfig{
		PolicyRuntimeConfig: cervoruntime.PolicyRuntimeConfig{
			TrustedUsers:    splitCSV(envValue("PROXY_TRUSTED_USERS")),
			DefaultExecutor: cervorules.Executor(canonicalEnvValue("PROXY_DEFAULT_PROVIDER")),
		},
		DefaultTimeout: defaultTimeout,
		BreakerPolicy:  breaker,
		RetryPolicy:    retry,
		Limits:         limits,
	}
	return cfg
}

func loadRouteOverridesFromEnv() map[cervorules.Operation]cervorules.Target {
	routes := map[cervorules.Operation]cervorules.Target{}
	for _, override := range envRouteOverrides {
		if service := serviceFromEnv(override.Env); service != "" {
			routes[override.Operation] = service
		}
	}
	return routes
}

func serviceFromEnv(key string) cervorules.Target {
	return cervorules.Target(canonicalEnvValue(key))
}

func loadExecutorFallbacksFromEnv() map[cervorules.Executor][]cervorules.Executor {
	if raw := envValue("PROXY_PROVIDER_FALLBACKS_JSON"); raw != "" {
		fallbacks := map[cervorules.Executor][]cervorules.Executor{}
		if err := json.Unmarshal([]byte(raw), &fallbacks); err != nil {
			slog.Warn("[CalvoProxy] invalid PROXY_PROVIDER_FALLBACKS_JSON", slog.Any("error", err))
		} else {
			return fallbacks
		}
	}
	return nil
}

func loadPolicyFlow(defaultTimeout time.Duration, failureThreshold int, cooldown time.Duration) (cervorules.Engine, ruleRuntimeConfig) {
	cfg := loadRuleConfig(defaultTimeout, failureThreshold, cooldown)
	flow, err := buildPolicyFlow(cfg)
	if err != nil {
		attrs := append([]slog.Attr{slog.String("fallback", "deny_all")}, policyErrorLogAttrs(err)...)
		slog.LogAttrs(context.Background(), slog.LevelError, "[CalvoProxy] policy flow compile failed", attrs...)
		return denyAllPolicyEngine("policy flow compile failed"), cfg
	}
	return flow, cfg
}

func generatedPolicyMetadata() cervoruntime.PolicyMetadata {
	return policyrules.NewPolicyFactory().Metadata()
}

func buildPolicyFlow(cfg ruleRuntimeConfig) (cervorules.Engine, error) {
	cfg = normalizeRuleConfig(cfg)
	factory := policyrules.NewPolicyFactory()
	effectiveConfig := mergePolicyRuntimeConfig(factory.DefaultConfig(), cfg.PolicyRuntimeConfig)
	return factory.Build(context.Background(), effectiveConfig)
}

func mergePolicyRuntimeConfig(base, override cervoruntime.PolicyRuntimeConfig) cervoruntime.PolicyRuntimeConfig {
	out := base.Clone()
	if len(override.TrustedUsers) > 0 {
		out.TrustedUsers = append([]string(nil), override.TrustedUsers...)
	}
	if override.DefaultExecutor != "" {
		out.DefaultExecutor = override.DefaultExecutor
	}
	for operation, target := range override.OperationTargets {
		if out.OperationTargets == nil {
			out.OperationTargets = map[cervorules.Operation]cervorules.Target{}
		}
		out.OperationTargets[operation] = target
	}
	for executor, fallbacks := range override.ExecutorFallbacks {
		if out.ExecutorFallbacks == nil {
			out.ExecutorFallbacks = map[cervorules.Executor][]cervorules.Executor{}
		}
		out.ExecutorFallbacks[executor] = append([]cervorules.Executor(nil), fallbacks...)
	}
	return out
}

func hasCustomRetry(policy RetryPolicy) bool {
	return policy.MaxAttempts != 0 ||
		len(policy.RetryHTTPStatuses) != 0 ||
		policy.RetryOnEOF ||
		policy.RetryOnTimeout ||
		policy.BackoffMin != 0 ||
		policy.BackoffMax != 0
}

func hasCustomLimits(limits Limits) bool {
	return limits.AllowStream ||
		limits.AllowTools ||
		limits.AllowImages ||
		limits.MaxBodyBytes != 0 ||
		limits.MaxTokens != 0
}

func normalizeRuleConfig(cfg ruleRuntimeConfig) ruleRuntimeConfig {
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = defaultPolicyTimeout
	}
	if cfg.DefaultExecutor == "" {
		cfg.DefaultExecutor = providerOpenRouter
	}
	if cfg.BreakerPolicy.FailureThreshold <= 0 {
		cfg.BreakerPolicy.FailureThreshold = 3
	}
	if cfg.BreakerPolicy.Cooldown <= 0 {
		cfg.BreakerPolicy.Cooldown = 60 * time.Second
	}
	if cfg.BreakerPolicy.Eligible == false {
		cfg.BreakerPolicy.Eligible = true
	}
	return cfg
}
