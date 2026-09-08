package main

import (
	"strings"
	"testing"
)

func envMap(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kv := range buildChildEnv() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

// The clamp is why context compaction could not finish: at 1024 the summary
// itself hit finish_reason=length, and an incomplete summary frees nothing, so
// the session died of overflow after three attempts. Measured 2026-09-08: all
// three profiles returned exactly 1024 output tokens no matter what the client
// asked for.
func TestCompletionClampLeavesRoomForACompactionSummary(t *testing.T) {
	got := envMap(t)["PROXY_MAX_COMPLETION_TOKENS"]
	if got == "" {
		t.Fatal("PROXY_MAX_COMPLETION_TOKENS is unset; the proxy default applies and this launcher no longer pins it")
	}
	if got == "1024" {
		t.Fatal("clamp is back at 1024: a compaction summary does not fit, so an oversized session cannot recover")
	}
	// Must stay under the OutputReserveTokens the chain's models declare (16384),
	// or the context filter admits requests whose output cannot fit.
	if got != "8192" {
		t.Fatalf("clamp = %s; expected 8192 (under the 16384 output reserve)", got)
	}
}

// The upstream key on this machine is rejected with "User not found"; inheriting
// it makes every OpenRouter attempt fail before the chain can try a healthy one.
func TestTheStaleUpstreamKeyIsNotInherited(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-stale-should-not-reach-the-child")
	if v, ok := envMap(t)["OPENROUTER_API_KEY"]; ok {
		t.Fatalf("OPENROUTER_API_KEY reached the child environment (%q)", v)
	}
}

// The tuning this launcher exists to apply must actually be present; a typo in
// a key name would silently fall back to the shipped defaults.
func TestTheLocalTuningIsApplied(t *testing.T) {
	env := envMap(t)
	for key, want := range map[string]string{
		"PORT":                            "8080",
		"GRPC_PORT":                       "19090",
		"PROXY_TOOL_RESULT_LIMIT":         "8192",
		"PROXY_REQUEST_TIMEOUT_SECONDS":   "90",
		"PROXY_TOTAL_TIMEOUT_SECONDS":     "180",
		"PROXY_STREAM_IDLE_TIMEOUT":       "20",
		"PROXY_STREAM_FIRST_BYTE_TIMEOUT": "3",
	} {
		if env[key] != want {
			t.Errorf("%s = %q, want %q", key, env[key], want)
		}
	}
}
