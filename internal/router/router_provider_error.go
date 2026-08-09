package router

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const openRouterDailyFreeQuotaPrefix = "OpenRouter daily free-model quota exhausted"

// openRouterDailyFreeQuotaMessage turns OpenRouter's account-wide daily free
// quota response into an operator-facing explanation. A generic "HTTP 429"
// makes clients look hung and encourages retries that cannot succeed; this
// quota is shared by every :free model until the advertised UTC reset.
func openRouterDailyFreeQuotaMessage(body string) (string, bool) {
	details, ok := openRouterDailyFreeQuotaDetails(body)
	return details.Message, ok
}

type dailyFreeQuotaDetails struct {
	Message string
	Reset   time.Time
}

func openRouterDailyFreeQuotaDetails(body string) (dailyFreeQuotaDetails, bool) {
	var parsed struct {
		Error struct {
			Code     int `json:"code"`
			Metadata struct {
				Headers     map[string]string `json:"headers"`
				LimitSource string            `json:"limit_source"`
			} `json:"metadata"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil ||
		parsed.Error.Code != 429 || parsed.Error.Metadata.LimitSource != "openrouter_free_tier_daily" {
		return dailyFreeQuotaDetails{}, false
	}

	limit := quotaHeader(parsed.Error.Metadata.Headers, "X-RateLimit-Limit")
	remaining := quotaHeader(parsed.Error.Metadata.Headers, "X-RateLimit-Remaining")
	usage := "no requests remaining"
	if limitValue, limitErr := strconv.ParseInt(limit, 10, 64); limitErr == nil {
		if remainingValue, remainingErr := strconv.ParseInt(remaining, 10, 64); remainingErr == nil && remainingValue >= 0 && remainingValue <= limitValue {
			usage = fmt.Sprintf("%d/%d", limitValue-remainingValue, limitValue)
		}
	}

	resetText := "at the next daily reset"
	var reset time.Time
	if resetMillis, err := strconv.ParseInt(quotaHeader(parsed.Error.Metadata.Headers, "X-RateLimit-Reset"), 10, 64); err == nil {
		reset = time.UnixMilli(resetMillis)
		resetText = reset.UTC().Format("2006-01-02 15:04 UTC")
		local := reset.Local()
		if _, offset := local.Zone(); offset != 0 {
			resetText += " (local time: " + local.Format("2006-01-02 15:04 -07:00") + ")"
		}
	}

	return dailyFreeQuotaDetails{
		Message: fmt.Sprintf("%s (%s). Resets: %s. Switching to another :free model will not help; wait for the reset or use a direct provider/paid model.", openRouterDailyFreeQuotaPrefix, usage, resetText),
		Reset:   reset,
	}, true
}

func quotaHeader(headers map[string]string, name string) string {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// isProviderRelayedError reports whether an upstream error body is OpenRouter
// relaying a failure from the PROVIDER it routed to, rather than rejecting the
// request itself.
//
// The distinction decides whether the chain advances, and getting it wrong is
// expensive in both directions:
//
//   - Treat a relayed error as terminal and one picky provider kills a request
//     every other model in the chain would have served.
//   - Treat a genuine account-level rejection as retryable and a bad API key
//     burns the whole chain, K times per request, hiding the one error the
//     operator needs to see.
//
// OpenRouter marks the difference in the body. A relayed failure carries the
// fixed message "Provider returned error" plus a metadata.provider_name naming
// who actually refused:
//
//	{"error":{"message":"Provider returned error","code":401,
//	          "metadata":{"provider_name":"Darkbloom",
//	                      "raw":"{\"error\":{\"code\":\"authentication_error\"...}}"}}}
//
// A rejection by OpenRouter itself has neither. Both observed in production:
// a 400 "at most 64 tools are allowed" and a 401 "invalid API key" from the
// same provider, on an account whose own key was valid at the time.
func isProviderRelayedError(body string) bool {
	if body == "" {
		return false
	}
	// Cheap reject before parsing: the marker is a fixed string, and error
	// bodies are read on every non-200.
	if !strings.Contains(body, "Provider returned error") {
		return false
	}

	var parsed struct {
		Error struct {
			Message  string `json:"message"`
			Metadata struct {
				ProviderName string `json:"provider_name"`
			} `json:"metadata"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		// Unparseable body that still contains the marker: not enough to act on.
		// Defaulting to "not relayed" keeps the previous, terminal behaviour.
		return false
	}
	if parsed.Error.Message != "Provider returned error" {
		return false
	}
	// provider_name is what makes it attributable to a downstream provider.
	// Without it there is nothing to distinguish this from OpenRouter's own
	// refusal, so leave the chain alone.
	return strings.TrimSpace(parsed.Error.Metadata.ProviderName) != ""
}
