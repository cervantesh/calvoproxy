package cervoobserve

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
)

var DefaultSecretKeys = []string{"token", "secret", "password", "passwd", "api_key", "apikey", "authorization", "cookie"}

func Truncate(value string, max int) string {
	trimmed := strings.TrimSpace(value)
	if max <= 0 || len(trimmed) <= max {
		return trimmed
	}
	return trimmed[:max]
}

func IsSecretKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	for _, candidate := range DefaultSecretKeys {
		if strings.Contains(lower, candidate) {
			return true
		}
	}
	return false
}

func RedactMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for k, v := range values {
		if IsSecretKey(k) {
			out[k] = "[REDACTED]"
		} else {
			out[k] = v
		}
	}
	return out
}

func Fingerprint(secret string) string {
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:])[:12]
}

func Attrs(service string, requestID string) []slog.Attr {
	attrs := make([]slog.Attr, 0, 2)
	if service = strings.TrimSpace(service); service != "" {
		attrs = append(attrs, slog.String("service", service))
	}
	if requestID = strings.TrimSpace(requestID); requestID != "" {
		attrs = append(attrs, slog.String("request_id", requestID))
	}
	return attrs
}
