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

// maxRequestBodyBytes is the cap applied to incoming request bodies via
// http.MaxBytesReader. Configurable with PROXY_MAX_BODY_BYTES; default 10 MiB.
func maxRequestBodyBytes() int64 {
	if raw := envValue("PROXY_MAX_BODY_BYTES"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 10 << 20
}

// maxResponseBytes caps how much of a NON-streaming upstream response we buffer
// into memory before writing it back, so a huge/malformed upstream body can't
// OOM the process (the request body is already capped separately). Configurable
// with PROXY_MAX_RESPONSE_BYTES; default 25 MiB.
func maxResponseBytes() int64 {
	if raw := envValue("PROXY_MAX_RESPONSE_BYTES"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 25 << 20
}

// maxErrorBodyBytes caps how much of an upstream ERROR response we read for the
// failure message. Error bodies are only used for classification/logging, so a
// small fixed cap is plenty and keeps a hostile upstream from flooding memory.
func maxErrorBodyBytes() int64 { return 1 << 20 }

// streamIdleTimeout bounds the gap BETWEEN chunks of a streamed (SSE) response:
// a live stream keeps flowing, but one that stalls with no bytes for this window
// is aborted so a hung upstream can't pin the connection forever. This replaces
// the old whole-request client timeout that used to cut long-but-live streams.
// Configurable with PROXY_STREAM_IDLE_TIMEOUT (seconds); default 120s.
func streamIdleTimeout() time.Duration {
	return envDurationSeconds("PROXY_STREAM_IDLE_TIMEOUT", 120*time.Second)
}

// streamMaxDuration is an absolute backstop on a single stream's total lifetime,
// so even a pathological keepalive that never idles cannot pin a goroutine
// indefinitely. Configurable with PROXY_STREAM_MAX_DURATION (seconds); default
// 30m. Set to 0 to disable the backstop (idle timeout still applies).
func streamMaxDuration() time.Duration {
	if raw := envValue("PROXY_STREAM_MAX_DURATION"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			if n <= 0 {
				return 0
			}
			return time.Duration(n) * time.Second
		}
	}
	return 30 * time.Minute
}

// totalTimeout is the overall wall-clock budget for a NON-streaming chat request
// across its whole fallback chain (distinct from the per-attempt timeout, so a
// slow first model can't consume the entire budget and starve fallbacks).
// Configurable with PROXY_TOTAL_TIMEOUT_SECONDS; default 120s (or the per-attempt
// timeout if that is somehow larger).
func totalTimeout(perAttempt time.Duration) time.Duration {
	if d := envDurationSeconds("PROXY_TOTAL_TIMEOUT_SECONDS", 0); d > 0 {
		return d
	}
	if def := 120 * time.Second; def > perAttempt {
		return def
	}
	return perAttempt
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
