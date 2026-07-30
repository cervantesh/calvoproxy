package router

import (
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

func loadRetryPolicyFromEnv() RetryPolicy {
	policy := RetryPolicy{}
	if raw := envValue("PROXY_RETRY_POLICY_JSON"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &policy); err != nil {
			slog.Warn("[CalvoProxy] invalid PROXY_RETRY_POLICY_JSON", slog.Any("error", err))
		}
	}
	return policy
}

func loadLimitsFromEnv() Limits {
	limits := Limits{}
	if raw := envValue("PROXY_LIMITS_JSON"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &limits); err != nil {
			slog.Warn("[CalvoProxy] invalid PROXY_LIMITS_JSON", slog.Any("error", err))
		}
	}
	if raw := envValue("PROXY_MAX_BODY_BYTES"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			limits.MaxBodyBytes = parsed
		}
	}
	return limits
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func envValue(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

func canonicalEnvValue(key string) string {
	return strings.ToLower(envValue(key))
}

func envBool(key string, fallback bool) bool {
	raw := canonicalEnvValue(key)
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func envInt(key string, fallback int) int {
	raw := envValue(key)
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDurationSeconds(key string, fallback time.Duration) time.Duration {
	raw := envValue(key)
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return time.Duration(parsed) * time.Second
}
