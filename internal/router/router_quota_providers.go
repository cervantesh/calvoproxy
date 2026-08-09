package router

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// providerQuotaObservation is the provider-adapter boundary for quota state.
// It deliberately contains no raw response body or provider error text: callers
// can persist and expose these normalized facts without leaking upstream data.
type providerQuotaObservation struct {
	Provider       providerID
	Model          string
	Scope          providerQuotaScope
	Dimension      providerQuotaDimension
	Window         providerQuotaWindow
	Limit          int64
	Remaining      int64
	HasLimit       bool
	HasRemaining   bool
	ResetAt        time.Time
	RetryAfter     time.Duration
	ObservedAt     time.Time
	Source         providerQuotaSource
	Classification providerQuotaClassification
	Exhausted      bool
}

type providerQuotaScope string
type providerQuotaDimension string
type providerQuotaWindow string
type providerQuotaSource string
type providerQuotaClassification string

const (
	providerQuotaScopeUnknown  providerQuotaScope = "unknown"
	providerQuotaScopeAccount  providerQuotaScope = "account"
	providerQuotaScopeFreePool providerQuotaScope = "free_pool"
	providerQuotaScopeModel    providerQuotaScope = "model"

	providerQuotaDimensionUnknown  providerQuotaDimension = "unknown"
	providerQuotaDimensionRequests providerQuotaDimension = "requests"
	providerQuotaDimensionTokens   providerQuotaDimension = "tokens"

	providerQuotaWindowUnknown providerQuotaWindow = "unknown"
	providerQuotaWindowMinute  providerQuotaWindow = "minute"
	providerQuotaWindowDay     providerQuotaWindow = "day"

	providerQuotaSourceHeader        providerQuotaSource = "provider_header"
	providerQuotaSourceErrorMetadata providerQuotaSource = "error_metadata"
	providerQuotaSourceStatusOnly    providerQuotaSource = "status_only"

	providerQuotaKnown   providerQuotaClassification = "known"
	providerQuotaUnknown providerQuotaClassification = "unknown"
)

// ledgerFact converts a normalized provider observation into the provider-
// neutral ledger contract. Unknown 429s intentionally return ok=false: they
// are useful for a short model cooldown, but must not create an invented hard
// quota bucket.
func (o providerQuotaObservation) ledgerFact() (QuotaBucketKey, QuotaObservation, bool) {
	if o.Classification != providerQuotaKnown || !o.HasLimit || !o.HasRemaining {
		return QuotaBucketKey{}, QuotaObservation{}, false
	}
	dimension := QuotaDimension(o.Dimension)
	if dimension != QuotaDimensionRequests && dimension != QuotaDimensionTokens {
		return QuotaBucketKey{}, QuotaObservation{}, false
	}
	window := QuotaWindow(o.Window)
	if window != QuotaWindowMinute && window != QuotaWindowHour && window != QuotaWindowDay {
		return QuotaBucketKey{}, QuotaObservation{}, false
	}
	modelOrPool := o.Model
	if o.Scope == providerQuotaScopeFreePool {
		modelOrPool = "free"
	}
	source := QuotaSourceProviderHeader
	if o.Source == providerQuotaSourceErrorMetadata {
		source = QuotaSourceProviderBody
	}
	return QuotaBucketKey{
			Provider: string(o.Provider), Scope: string(o.Scope), ModelOrPool: modelOrPool,
			Dimension: dimension, Window: window,
		}, QuotaObservation{
			Limit: o.Limit, Remaining: o.Remaining, ResetAt: o.ResetAt,
			Source: source, Confidence: QuotaConfidenceAuthoritative, ObservedAt: o.ObservedAt,
		}, true
}

// observeProviderQuota normalizes only documented provider signals. A bare 429
// remains an unknown, model-local observation; it must not be interpreted as a
// provider/account-wide outage.
func observeProviderQuota(provider providerID, model string, status int, headers http.Header, body []byte, observedAt time.Time) []providerQuotaObservation {
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	retryAfter := parseRetryAfterAt(headerValue(headers, "Retry-After"), observedAt)

	var observations []providerQuotaObservation
	switch provider {
	case providerOpenRouter:
		observations = observeOpenRouterQuota(model, status, headers, body, observedAt)
	case providerCerebras:
		observations = observeCerebrasQuota(model, headers, observedAt)
	case providerGroq:
		observations = observeGroqQuota(model, headers, observedAt)
	}

	for i := range observations {
		observations[i].RetryAfter = retryAfter
	}
	if len(observations) == 0 && status == http.StatusTooManyRequests {
		observations = append(observations, providerQuotaObservation{
			Provider: provider, Model: model, Scope: providerQuotaScopeUnknown,
			Dimension: providerQuotaDimensionUnknown, Window: providerQuotaWindowUnknown,
			RetryAfter: retryAfter, ObservedAt: observedAt,
			Source: providerQuotaSourceStatusOnly, Classification: providerQuotaUnknown,
			Exhausted: true,
		})
	}
	return observations
}

func observeOpenRouterQuota(model string, status int, headers http.Header, body []byte, now time.Time) []providerQuotaObservation {
	// OpenRouter's free-tier daily rejection embeds the authoritative headers in
	// error.metadata and explicitly names the account-wide free pool.
	if status == http.StatusTooManyRequests {
		var parsed struct {
			Error struct {
				Code     int `json:"code"`
				Metadata struct {
					Headers     map[string]string `json:"headers"`
					LimitSource string            `json:"limit_source"`
				} `json:"metadata"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &parsed) == nil && parsed.Error.Code == http.StatusTooManyRequests &&
			parsed.Error.Metadata.LimitSource == "openrouter_free_tier_daily" {
			return observationsFromFields(providerOpenRouter, model, providerQuotaScopeFreePool,
				providerQuotaDimensionRequests, providerQuotaWindowDay,
				quotaHeader(parsed.Error.Metadata.Headers, "X-RateLimit-Limit"),
				quotaHeader(parsed.Error.Metadata.Headers, "X-RateLimit-Remaining"),
				quotaHeader(parsed.Error.Metadata.Headers, "X-RateLimit-Reset"),
				now, providerQuotaSourceErrorMetadata, resetUnixMilliseconds)
		}
	}

	// Outside the explicitly identified daily-free response, OpenRouter's
	// generic fields do not identify their window. Preserve that uncertainty.
	return observationsFromFields(providerOpenRouter, model, providerQuotaScopeAccount,
		providerQuotaDimensionRequests, providerQuotaWindowUnknown,
		headerValue(headers, "X-RateLimit-Limit"), headerValue(headers, "X-RateLimit-Remaining"),
		headerValue(headers, "X-RateLimit-Reset"), now, providerQuotaSourceHeader, resetUnixMilliseconds)
}

func observeCerebrasQuota(model string, headers http.Header, now time.Time) []providerQuotaObservation {
	var out []providerQuotaObservation
	out = append(out, observationsFromFields(providerCerebras, model, providerQuotaScopeModel,
		providerQuotaDimensionRequests, providerQuotaWindowDay,
		headerValue(headers, "X-RateLimit-Limit-Requests-Day"),
		headerValue(headers, "X-RateLimit-Remaining-Requests-Day"),
		headerValue(headers, "X-RateLimit-Reset-Requests-Day"), now, providerQuotaSourceHeader, resetDurationOrTimestamp)...)
	out = append(out, observationsFromFields(providerCerebras, model, providerQuotaScopeModel,
		providerQuotaDimensionTokens, providerQuotaWindowMinute,
		headerValue(headers, "X-RateLimit-Limit-Tokens-Minute"),
		headerValue(headers, "X-RateLimit-Remaining-Tokens-Minute"),
		headerValue(headers, "X-RateLimit-Reset-Tokens-Minute"), now, providerQuotaSourceHeader, resetDurationOrTimestamp)...)
	return out
}

func observeGroqQuota(model string, headers http.Header, now time.Time) []providerQuotaObservation {
	var out []providerQuotaObservation
	// Groq documents the unsuffixed request fields as RPD and token fields as TPM.
	out = append(out, observationsFromFields(providerGroq, model, providerQuotaScopeModel,
		providerQuotaDimensionRequests, providerQuotaWindowDay,
		headerValue(headers, "X-RateLimit-Limit-Requests"),
		headerValue(headers, "X-RateLimit-Remaining-Requests"),
		headerValue(headers, "X-RateLimit-Reset-Requests"), now, providerQuotaSourceHeader, resetDurationOrTimestamp)...)
	out = append(out, observationsFromFields(providerGroq, model, providerQuotaScopeModel,
		providerQuotaDimensionTokens, providerQuotaWindowMinute,
		headerValue(headers, "X-RateLimit-Limit-Tokens"),
		headerValue(headers, "X-RateLimit-Remaining-Tokens"),
		headerValue(headers, "X-RateLimit-Reset-Tokens"), now, providerQuotaSourceHeader, resetDurationOrTimestamp)...)
	return out
}

type resetParser func(string, time.Time) (time.Time, bool)

func observationsFromFields(provider providerID, model string, scope providerQuotaScope, dimension providerQuotaDimension, window providerQuotaWindow, limitRaw, remainingRaw, resetRaw string, now time.Time, source providerQuotaSource, parseReset resetParser) []providerQuotaObservation {
	limit, hasLimit := parseNonNegativeInt64(limitRaw)
	remaining, hasRemaining := parseNonNegativeInt64(remainingRaw)
	if hasLimit && hasRemaining && remaining > limit {
		remaining = limit
	}
	resetAt, hasReset := parseReset(resetRaw, now)
	if !hasLimit && !hasRemaining && !hasReset {
		return nil
	}
	return []providerQuotaObservation{{
		Provider: provider, Model: model, Scope: scope, Dimension: dimension, Window: window,
		Limit: limit, Remaining: remaining, HasLimit: hasLimit, HasRemaining: hasRemaining,
		ResetAt: resetAt, ObservedAt: now, Source: source, Classification: providerQuotaKnown,
		Exhausted: hasRemaining && remaining == 0,
	}}
}

func parseNonNegativeInt64(raw string) (int64, bool) {
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	return v, err == nil && v >= 0
}

func headerValue(headers http.Header, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func parseRetryAfterAt(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if seconds, ok := parseNonNegativeInt64(raw); ok {
		if seconds <= int64((time.Duration(1<<63-1))/time.Second) {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if parsed, err := http.ParseTime(raw); err == nil && parsed.After(now) {
		return parsed.Sub(now)
	}
	return 0
}

func resetUnixMilliseconds(raw string, _ time.Time) (time.Time, bool) {
	millis, ok := parseNonNegativeInt64(raw)
	if !ok || millis == 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(millis), true
}

func resetDurationOrTimestamp(raw string, now time.Time) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if duration, err := time.ParseDuration(raw); err == nil && duration >= 0 {
		return now.Add(duration), true
	}
	if parsed, err := http.ParseTime(raw); err == nil {
		return parsed, true
	}
	// Accept Unix seconds/milliseconds defensively. Small bare integers are
	// durations in seconds, which matches provider reset examples.
	n, ok := parseNonNegativeInt64(raw)
	if !ok {
		return time.Time{}, false
	}
	switch {
	case n >= 1_000_000_000_000:
		return time.UnixMilli(n), true
	case n >= 1_000_000_000:
		return time.Unix(n, 0), true
	default:
		if n > int64((time.Duration(1<<63-1))/time.Second) {
			return time.Time{}, false
		}
		return now.Add(time.Duration(n) * time.Second), true
	}
}
