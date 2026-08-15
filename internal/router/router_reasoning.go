package router

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// headerReasoning is the per-request override. A query parameter is accepted
// alongside it for the same reason profile selection accepts one: some clients
// can reach the URL but not the headers.
const headerReasoning = "X-Cervo-Reasoning"

// Reasoning-effort control.
//
// CalvoProxy already forwards whatever the caller puts in the body, so a client
// that knows what it wants could always set reasoning itself. What was missing
// is the proxy DECIDING: a profile is a statement of intent ("this is the
// reasoning chain", "this is the cheap bulk chain"), and the amount of thinking
// that intent deserves belongs with the profile rather than in every client.
//
// Resolution order, highest first:
//  1. the caller's own body (reasoning_effort / reasoning.effort) — never
//     overridden, because an explicit request beats an operator default,
//  2. the per-request header (X-Cervo-Reasoning),
//  3. the profile default from model-policy.json "Reasoning",
//  4. the global PROXY_REASONING_EFFORT floor,
//  5. nothing — the model's own default applies.
//
// Injection is capability-gated: the effort is only written for models the
// catalog says accept it, so enabling this can never turn a working free model
// into a 400.

type reasoningEffort string

const (
	reasoningEffortLow    reasoningEffort = "low"
	reasoningEffortMedium reasoningEffort = "medium"
	reasoningEffortHigh   reasoningEffort = "high"
)

func parseReasoningEffort(raw string) (reasoningEffort, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "low":
		return reasoningEffortLow, true
	case "medium", "med":
		return reasoningEffortMedium, true
	case "high":
		return reasoningEffortHigh, true
	default:
		return "", false
	}
}

// reasoningProfiles maps a profile name to the effort its requests should carry.
type reasoningProfiles map[string]reasoningEffort

type reasoningProfilesSchema struct {
	Reasoning map[string]string `json:"Reasoning"`
}

func parseReasoningProfiles(data []byte) (reasoningProfiles, bool) {
	var schema reasoningProfilesSchema
	if err := json.Unmarshal(data, &schema); err != nil || schema.Reasoning == nil {
		return nil, false
	}
	out := make(reasoningProfiles, len(schema.Reasoning))
	for profile, raw := range schema.Reasoning {
		profile = strings.TrimSpace(profile)
		if profile == "" {
			continue
		}
		effort, ok := parseReasoningEffort(raw)
		if !ok {
			slog.Warn("[CalvoProxy] ignoring invalid reasoning effort",
				slog.String("profile", profile), slog.String("effort", raw))
			continue
		}
		out[profile] = effort
	}
	return out, true
}

// loadReasoningProfiles reads the per-profile efforts from the same
// hot-reloadable model-policy.json as the model chains, so effort and chain are
// edited together and reload in the same pass.
func loadReasoningProfiles() reasoningProfiles {
	for _, path := range modelPolicyFileCandidates() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		profiles, _ := parseReasoningProfiles(data)
		return profiles
	}
	profiles, _ := parseReasoningProfiles(defaultModelConfigJSON)
	return profiles
}

func cloneReasoningProfiles(in reasoningProfiles) reasoningProfiles {
	if in == nil {
		return nil
	}
	out := make(reasoningProfiles, len(in))
	for profile, effort := range in {
		out[profile] = effort
	}
	return out
}

type reasoningEffortKey struct{}

// WithReasoningEffort records the per-request override parsed from the
// X-Cervo-Reasoning header at the HTTP layer.
func WithReasoningEffort(ctx context.Context, effort reasoningEffort) context.Context {
	return context.WithValue(ctx, reasoningEffortKey{}, effort)
}

// withRequestReasoningEffort lifts the per-request override off the HTTP
// request and into the context the chain carries.
func withRequestReasoningEffort(ctx context.Context, r *http.Request) context.Context {
	if r == nil {
		return ctx
	}
	candidates := []string{r.Header.Get(headerReasoning)}
	if r.URL != nil {
		candidates = append(candidates, r.URL.Query().Get("reasoning"))
	}
	for _, raw := range candidates {
		if effort, ok := parseReasoningEffort(raw); ok {
			return WithReasoningEffort(ctx, effort)
		}
	}
	return ctx
}

func requestReasoningEffort(ctx context.Context) (reasoningEffort, bool) {
	if ctx == nil {
		return "", false
	}
	effort, ok := ctx.Value(reasoningEffortKey{}).(reasoningEffort)
	return effort, ok && effort != ""
}

// bodyCarriesReasoning reports whether the caller already expressed an opinion.
// Any recognised shape counts, including one this proxy would not have written
// itself (a token budget, say) — the point is not to second-guess a caller who
// has clearly configured this deliberately.
func bodyCarriesReasoning(body map[string]interface{}) bool {
	if body == nil {
		return false
	}
	if _, ok := body["reasoning_effort"]; ok {
		return true
	}
	if _, ok := body["reasoning"]; ok {
		return true
	}
	return false
}

// resolveReasoningEffort applies the precedence order for one attempt.
func (s *RouterService) resolveReasoningEffort(ctx context.Context, profile string) (reasoningEffort, bool) {
	if effort, ok := requestReasoningEffort(ctx); ok {
		return effort, true
	}
	if s != nil {
		if effort, ok := s.getReasoningProfiles()[strings.TrimSpace(profile)]; ok {
			return effort, true
		}
	}
	return parseReasoningEffort(envValue("PROXY_REASONING_EFFORT"))
}

// applyReasoningEffort writes the effort in the shape the model accepts.
// Models advertising reasoning_effort get the flat OpenAI-style field; the rest
// get OpenRouter's normalized reasoning object. A model advertising neither is
// left alone — that is the whole point of gating on capability.
func applyReasoningEffort(body map[string]interface{}, effort reasoningEffort, model string, caps *capabilityIndex) {
	if body == nil || effort == "" || caps == nil {
		return
	}
	switch {
	case caps.satisfies(model, []string{capReasoningEffort}):
		body["reasoning_effort"] = string(effort)
	case caps.satisfies(model, []string{capReasoning}):
		body["reasoning"] = map[string]interface{}{"effort": string(effort)}
	}
}
