package router

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestObserveProviderQuota_OpenRouterDailyFreePoolMetadata(t *testing.T) {
	now := time.Date(2026, 8, 8, 20, 0, 0, 0, time.UTC)
	reset := now.Add(4 * time.Hour)
	body := []byte(`{"error":{"code":429,"message":"sensitive provider text","metadata":{"limit_source":"openrouter_free_tier_daily","headers":{"x-ratelimit-limit":"1000","X-RateLimit-Remaining":"0","x-ratelimit-reset":"` + formatInt(reset.UnixMilli()) + `"}}}}`)

	got := observeProviderQuota(providerOpenRouter, "openai/gpt-oss-20b:free", http.StatusTooManyRequests,
		http.Header{"Retry-After": []string{"17"}}, body, now)

	if len(got) != 1 {
		t.Fatalf("observations = %d, want 1: %+v", len(got), got)
	}
	observation := got[0]
	if observation.Scope != providerQuotaScopeFreePool || observation.Dimension != providerQuotaDimensionRequests || observation.Window != providerQuotaWindowDay {
		t.Errorf("unexpected normalized key: %+v", observation)
	}
	if !observation.HasLimit || observation.Limit != 1000 || !observation.HasRemaining || observation.Remaining != 0 || !observation.Exhausted {
		t.Errorf("unexpected counters: %+v", observation)
	}
	if !observation.ResetAt.Equal(reset) || observation.RetryAfter != 17*time.Second {
		t.Errorf("unexpected reset/retry: %+v", observation)
	}
	if observation.Source != providerQuotaSourceErrorMetadata || observation.Classification != providerQuotaKnown {
		t.Errorf("unexpected provenance: %+v", observation)
	}
	key, fact, ok := observation.ledgerFact()
	if !ok || key.ModelOrPool != "free" || key.Scope != string(providerQuotaScopeFreePool) || fact.Confidence != QuotaConfidenceAuthoritative {
		t.Errorf("ledger conversion lost free-pool identity: key=%+v observation=%+v ok=%v", key, fact, ok)
	}
}

func TestObserveProviderQuota_OpenRouterGenericHeadersPreserveUnknownWindow(t *testing.T) {
	now := time.Date(2026, 8, 8, 20, 0, 0, 0, time.UTC)
	headers := http.Header{}
	headers.Set("X-RateLimit-Limit", "20")
	headers.Set("X-RateLimit-Remaining", "7")
	headers.Set("X-RateLimit-Reset", "1786222800000")

	got := observeProviderQuota(providerOpenRouter, "some:model", http.StatusOK, headers, nil, now)
	if len(got) != 1 || got[0].Window != providerQuotaWindowUnknown || got[0].Scope != providerQuotaScopeAccount {
		t.Fatalf("generic OpenRouter fields must not invent a window: %+v", got)
	}
	if got[0].Exhausted || got[0].Remaining != 7 {
		t.Errorf("unexpected remaining state: %+v", got[0])
	}
}

func TestObserveProviderQuota_CerebrasParsesIndependentDimensions(t *testing.T) {
	now := time.Date(2026, 8, 8, 20, 0, 0, 0, time.UTC)
	headers := http.Header{}
	headers.Set("X-RateLimit-Limit-Requests-Day", "14400")
	headers.Set("X-RateLimit-Remaining-Requests-Day", "14300")
	headers.Set("X-RateLimit-Reset-Requests-Day", "4h")
	headers.Set("X-RateLimit-Limit-Tokens-Minute", "64000")
	headers.Set("X-RateLimit-Remaining-Tokens-Minute", "0")
	headers.Set("X-RateLimit-Reset-Tokens-Minute", "1m2.5s")

	got := observeProviderQuota(providerCerebras, "gpt-oss-120b", http.StatusTooManyRequests, headers, nil, now)
	if len(got) != 2 {
		t.Fatalf("observations = %d, want request and token buckets: %+v", len(got), got)
	}
	requests, tokens := got[0], got[1]
	if requests.Dimension != providerQuotaDimensionRequests || requests.Window != providerQuotaWindowDay || requests.Remaining != 14300 || !requests.ResetAt.Equal(now.Add(4*time.Hour)) {
		t.Errorf("request bucket = %+v", requests)
	}
	if tokens.Dimension != providerQuotaDimensionTokens || tokens.Window != providerQuotaWindowMinute || !tokens.Exhausted || !tokens.ResetAt.Equal(now.Add(time.Minute+2500*time.Millisecond)) {
		t.Errorf("token bucket = %+v", tokens)
	}
}

func TestObserveProviderQuota_GroqMapsDocumentedWindows(t *testing.T) {
	now := time.Date(2026, 8, 8, 20, 0, 0, 0, time.UTC)
	headers := http.Header{
		"x-ratelimit-limit-requests":     []string{"1000"}, // exercise case-insensitive direct maps
		"x-ratelimit-remaining-requests": []string{"999"},
		"x-ratelimit-reset-requests":     []string{"3h12m"},
		"x-ratelimit-limit-tokens":       []string{"8000"},
		"x-ratelimit-remaining-tokens":   []string{"4000"},
		"x-ratelimit-reset-tokens":       []string{"12s"},
	}

	got := observeProviderQuota(providerGroq, "openai/gpt-oss-120b", http.StatusOK, headers, nil, now)
	if len(got) != 2 {
		t.Fatalf("observations = %d, want 2: %+v", len(got), got)
	}
	if got[0].Window != providerQuotaWindowDay || got[0].Limit != 1000 || !got[0].ResetAt.Equal(now.Add(3*time.Hour+12*time.Minute)) {
		t.Errorf("Groq request bucket = %+v", got[0])
	}
	if got[1].Window != providerQuotaWindowMinute || got[1].Limit != 8000 || got[1].Remaining != 4000 {
		t.Errorf("Groq token bucket = %+v", got[1])
	}
}

func TestObserveProviderQuota_Bare429IsUnknownAndModelLocal(t *testing.T) {
	now := time.Date(2026, 8, 8, 20, 0, 0, 0, time.UTC)
	got := observeProviderQuota(providerGroq, "model-a", http.StatusTooManyRequests,
		http.Header{"Retry-After": []string{"Sat, 08 Aug 2026 20:00:30 GMT"}},
		[]byte(`{"error":"do not retain this detail"}`), now)

	if len(got) != 1 {
		t.Fatalf("observations = %d, want 1: %+v", len(got), got)
	}
	observation := got[0]
	if observation.Scope != providerQuotaScopeUnknown || observation.Classification != providerQuotaUnknown || observation.Dimension != providerQuotaDimensionUnknown {
		t.Errorf("bare 429 was over-classified: %+v", observation)
	}
	if observation.RetryAfter != 30*time.Second || !observation.Exhausted {
		t.Errorf("bare 429 cooldown = %+v", observation)
	}
	if _, _, ok := observation.ledgerFact(); ok {
		t.Error("an unknown 429 must not create an invented hard quota bucket")
	}
}

func TestObserveProviderQuota_RejectsMalformedAndNegativeFields(t *testing.T) {
	now := time.Date(2026, 8, 8, 20, 0, 0, 0, time.UTC)
	headers := http.Header{}
	headers.Set("X-RateLimit-Limit-Requests", "secret")
	headers.Set("X-RateLimit-Remaining-Requests", "-1")
	headers.Set("X-RateLimit-Reset-Requests", "yesterday-ish")

	if got := observeProviderQuota(providerGroq, "model-a", http.StatusOK, headers, nil, now); len(got) != 0 {
		t.Fatalf("malformed headers should produce no facts: %+v", got)
	}
}

func TestParseRetryAfterAtRejectsPastDateAndNegativeSeconds(t *testing.T) {
	now := time.Date(2026, 8, 8, 20, 0, 0, 0, time.UTC)
	for _, raw := range []string{"-3", "9223372036854775807", "Sat, 08 Aug 2026 19:59:59 GMT", "garbage"} {
		if got := parseRetryAfterAt(raw, now); got != 0 {
			t.Errorf("parseRetryAfterAt(%q) = %v, want 0", raw, got)
		}
	}
}

func formatInt(v int64) string { return strconv.FormatInt(v, 10) }
