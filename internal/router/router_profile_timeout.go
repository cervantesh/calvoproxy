package router

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Per-profile attempt timeouts.
//
// The per-attempt deadline used to be one number for the whole proxy. That is
// wrong in both directions at once: a coding turn may legitimately generate for
// a minute, while a session title has no business taking more than a few
// seconds. With a single value tuned for the slow case, one hung upstream costs
// every profile the same wait — measured on this proxy, a stalled vision model
// held a request for 91 seconds before the chain moved on and answered in one.
//
// A profile is already the place where "what kind of work is this" is declared,
// so it is also the right place to say how long one attempt may take.
//
// Overrides only ever SHRINK the effective deadline. The transport's
// ResponseHeaderTimeout is fixed at the global request timeout, so a longer
// per-profile value could not be honoured for header arrival anyway; promising
// it in config and silently capping it would be worse than refusing it.
type profileTimeouts map[string]time.Duration

type profileTimeoutsSchema struct {
	Timeouts map[string]float64 `json:"Timeouts"`
}

// maxProfileTimeoutSeconds bounds a config value so a typo (say, milliseconds
// pasted into a seconds field) cannot park a request for hours.
const maxProfileTimeoutSeconds = 600

func parseProfileTimeouts(data []byte) (profileTimeouts, bool) {
	var schema profileTimeoutsSchema
	if err := json.Unmarshal(data, &schema); err != nil || schema.Timeouts == nil {
		return nil, false
	}
	out := make(profileTimeouts, len(schema.Timeouts))
	for profile, seconds := range schema.Timeouts {
		profile = strings.TrimSpace(profile)
		if profile == "" {
			continue
		}
		if seconds <= 0 || seconds > maxProfileTimeoutSeconds {
			slog.Warn("[CalvoProxy] ignoring out-of-range profile timeout",
				slog.String("profile", profile), slog.Float64("seconds", seconds))
			continue
		}
		out[profile] = time.Duration(seconds * float64(time.Second))
	}
	return out, true
}

// loadProfileTimeouts reads the per-profile deadlines from the same
// hot-reloadable model-policy.json as the chains, so a profile's models and its
// patience are edited together and reload in the same pass.
func loadProfileTimeouts() profileTimeouts {
	for _, path := range modelPolicyFileCandidates() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		timeouts, _ := parseProfileTimeouts(data)
		return timeouts
	}
	timeouts, _ := parseProfileTimeouts(defaultModelConfigJSON)
	return timeouts
}

func cloneProfileTimeouts(in profileTimeouts) profileTimeouts {
	if in == nil {
		return nil
	}
	out := make(profileTimeouts, len(in))
	for profile, d := range in {
		out[profile] = d
	}
	return out
}

// attemptTimeoutForProfile narrows the deadline the caller already computed.
// It never widens it: `current` is the ceiling established by the global
// configuration and the policy decision.
func (s *RouterService) attemptTimeoutForProfile(profile string, current time.Duration) time.Duration {
	if s == nil {
		return current
	}
	override, ok := s.getProfileTimeouts()[strings.TrimSpace(profile)]
	if !ok || override <= 0 {
		return current
	}
	if current > 0 && override >= current {
		return current
	}
	return override
}
