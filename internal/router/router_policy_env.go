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

// legacyEnvNames are settings still read under the pre-rename `CERVO_` prefix.
// The project is CalvoProxy and everything else is `PROXY_`, so these are the
// last inconsistency from the rename.
//
// They cannot simply be renamed here: the vendored `cervo-*` modules read
// several of the same names independently, and `vendor/` is generated from the
// source monorepo rather than edited in place. Renaming only this repo's reads
// would let the proxy honor a new name while the library kept reading the old
// one — the same setting taking effect in one half of the system and not the
// other, which is worse than the inconsistency it replaced.
//
// So instead of renaming, bridge: accept `PROXY_<X>` and, when it is set and
// the legacy name is not, publish it back into the process environment under
// `CERVO_<X>`. Every existing read site — ours and the vendored library's —
// keeps working untouched, and the two can no longer disagree.
var legacyEnvNames = []string{
	"CERVO_MODEL",
	"CERVO_MODEL_ALIASES_JSON",
	"CERVO_MODEL_ALLOWED",
	"CERVO_MODEL_DEFAULT",
	"CERVO_MODEL_DEFAULT_PROFILE",
	"CERVO_MODEL_POLICY_MODE",
	"CERVO_MODEL_POLICY_STRICT",
	"CERVO_MODEL_PROFILES_JSON",
	"CERVO_POLICY_DEBUG_INCLUDE_TRACE",
	"CERVO_POLICY_LOG_ENABLED",
	"CERVO_POLICY_LOG_LEVEL",
	"CERVO_POLICY_METRICS_ENABLED",
	"CERVO_POLICY_OBSERVATION_SAMPLE_RATE",
	"CERVO_POLICY_TRACE_ENABLED",
	// Read only by the vendored modules, exposed here for a consistent surface.
	"CERVO_DEFAULT_BEARER",
	"CERVO_FORWARD_AUTH",
	"CERVO_HTTP_BASE_URL",
	"CERVO_HTTP_TIMEOUT",
	"CERVO_HTTP_TOKEN",
}

// bridgeLegacyPolicyEnv maps PROXY_<X> onto CERVO_<X> for the names above.
// The legacy name always wins when both are set, so an existing deployment
// cannot change behavior by upgrading. Idempotent; call before reading policy.
func bridgeLegacyPolicyEnv() {
	for _, legacy := range legacyEnvNames {
		if envValue(legacy) != "" {
			continue // explicitly set under the old name: leave it alone
		}
		modern := "PROXY_" + strings.TrimPrefix(legacy, "CERVO_")
		if v := envValue(modern); v != "" {
			_ = os.Setenv(legacy, v)
		}
	}
}

// streamFirstEventTimeout bounds how long a STREAMING attempt may go without
// producing its first real SSE event before the chain abandons it and tries the
// next model. Default 0 = disabled.
//
// This is not PROXY_STREAM_IDLE_TIMEOUT: that bounds the gap BETWEEN chunks once
// streaming has begun. This one bounds only the initial wait, and it fires
// strictly before any byte reaches the client — which is what makes advancing
// the chain legal at all (after headers are written there is no fallback).
//
// Streaming only, deliberately. On a non-streaming request the upstream headers
// arrive when generation is essentially finished, so a short budget there would
// abort perfectly good long answers; those are already bounded per attempt by
// PROXY_REQUEST_TIMEOUT_SECONDS.
func streamFirstEventTimeout() time.Duration {
	return envDurationSeconds("PROXY_STREAM_FIRST_BYTE_TIMEOUT", 0)
}
