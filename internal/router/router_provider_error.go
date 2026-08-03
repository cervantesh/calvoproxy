package router

import (
	"encoding/json"
	"strings"
)

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
