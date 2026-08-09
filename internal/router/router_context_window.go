package router

import (
	"encoding/json"
	"math"
	"os"
	"strings"
)

// estimateRequestContext deliberately uses a more conservative byte/token
// ratio than quota accounting. Context rejection must prefer a false positive
// over spending an upstream attempt on a request that cannot fit.
func estimateRequestContext(body map[string]interface{}) QuotaEstimate {
	estimate := estimateRequestQuota(body)
	encoded, _ := json.Marshal(body)
	estimate.InputTokens = int64(math.Ceil(float64(len(encoded)) / 3.0))
	if estimate.InputTokens < 1 {
		estimate.InputTokens = 1
	}
	estimate.Tokens = estimate.InputTokens + estimate.OutputTokens
	return estimate
}

type contextWindow struct {
	ContextTokens       int64 `json:"ContextTokens"`
	OutputReserveTokens int64 `json:"OutputReserveTokens,omitempty"`
}

type contextWindowIndex map[string]contextWindow

type contextWindowsSchema struct {
	ContextWindows map[string]map[string]contextWindow `json:"ContextWindows"`
}

func providerModelKey(provider providerID, model string) string {
	return strings.ToLower(strings.TrimSpace(string(provider))) + "\x00" + strings.TrimSpace(model)
}

func parseContextWindows(data []byte) contextWindowIndex {
	var schema contextWindowsSchema
	if json.Unmarshal(data, &schema) != nil {
		return nil
	}
	out := contextWindowIndex{}
	for provider, models := range schema.ContextWindows {
		for model, window := range models {
			if window.ContextTokens <= 0 {
				continue
			}
			if window.OutputReserveTokens < 0 {
				window.OutputReserveTokens = 0
			}
			out[providerModelKey(providerID(provider), model)] = window
		}
	}
	return out
}

func loadContextWindows() contextWindowIndex {
	base := parseContextWindows(defaultModelConfigJSON)
	for _, path := range modelPolicyFileCandidates() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for key, window := range parseContextWindows(data) {
			base[key] = window
		}
		break
	}
	return base
}

func (s *RouterService) getContextWindows() contextWindowIndex {
	s.policyMu.RLock()
	defer s.policyMu.RUnlock()
	return s.contextWindows
}

func (s *RouterService) filterContextFit(attempts []modelAttempt, estimate QuotaEstimate) ([]modelAttempt, int) {
	windows := s.getContextWindows()
	if len(windows) == 0 {
		return attempts, 0
	}
	out := make([]modelAttempt, 0, len(attempts))
	excluded := 0
	for _, attempt := range attempts {
		window, known := windows[providerModelKey(attempt.Provider, attempt.Model)]
		if !known {
			out = append(out, attempt)
			continue
		}
		output := estimate.OutputTokens
		if !estimate.OutputExplicit && window.OutputReserveTokens > 0 {
			output = window.OutputReserveTokens
		}
		if estimate.InputTokens+output > window.ContextTokens {
			excluded++
			continue
		}
		out = append(out, attempt)
	}
	return out, excluded
}
