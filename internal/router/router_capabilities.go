package router

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Capability-aware routing.
//
// Some free models can accept images ("vision") or do function calling
// ("tools") and some cannot. The capabilityIndex maps a model id to the set of
// capabilities it supports, and the router filters the chain to models that
// satisfy what a request needs (images ⇒ vision, tools present ⇒ tools) before
// breaker/scoring/fallback. Sources are HYBRID and merged (overrides win):
//   - auto-derived from OpenRouter GET /models (input_modalities/supported_parameters),
//   - manual overrides from model-policy.json "Capabilities" (authoritative;
//     "!cap" denies a capability the auto data wrongly reported).
//
// Filtering is FAIL-CLOSED: a model with no known capability data does NOT
// satisfy a required capability (so we never silently route images/tools to an
// incapable model). Plain text / no-tools requests require nothing and are never
// filtered — zero behaviour change.
const (
	capVision = "vision"
	capTools  = "tools"
	// capReasoning / capReasoningEffort gate reasoning-effort injection. They are
	// derived from the same supported_parameters list as capTools, so enabling a
	// profile-level effort can never hand a parameter to a model that would
	// reject it. capReasoningEffort is the narrower flat OpenAI-style field;
	// capReasoning is OpenRouter's normalized reasoning object.
	capReasoning       = "reasoning"
	capReasoningEffort = "reasoning_effort"
)

type capabilityIndex struct {
	mu        sync.RWMutex
	caps      map[string]map[string]bool // published snapshot: model → cap → true
	overrides map[string][]string        // raw overrides (re-merged on refresh)
	auto      map[string]map[string]bool // last auto-derived snapshot
	warned    sync.Map                   // model → struct{}, for log-once on unknown
}

func newCapabilityIndex(overrides map[string][]string) *capabilityIndex {
	idx := &capabilityIndex{
		overrides: overrides,
		auto:      map[string]map[string]bool{},
	}
	idx.rebuild()
	return idx
}

// rebuild recomputes the published snapshot from auto ∪ overrides under the lock.
// Caller may hold or not hold mu — it locks internally; callers that already
// mutated auto/overrides under the lock should call rebuildLocked instead.
func (idx *capabilityIndex) rebuild() {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.rebuildLocked()
}

func (idx *capabilityIndex) rebuildLocked() {
	merged := make(map[string]map[string]bool, len(idx.auto))
	for model, set := range idx.auto {
		cp := make(map[string]bool, len(set))
		for c := range set {
			cp[c] = true
		}
		merged[model] = cp
	}
	for model, list := range idx.overrides {
		set := merged[model]
		if set == nil {
			set = map[string]bool{}
			merged[model] = set
		}
		for _, c := range list {
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			if strings.HasPrefix(c, "!") {
				delete(set, strings.TrimPrefix(c, "!"))
			} else {
				set[c] = true
			}
		}
	}
	idx.caps = merged
}

// setOverrides replaces the manual overrides and re-merges (hot-reload path).
func (idx *capabilityIndex) setOverrides(overrides map[string][]string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.overrides = overrides
	idx.rebuildLocked()
}

// setAuto replaces the auto-derived snapshot and re-merges (refresh path).
func (idx *capabilityIndex) setAuto(auto map[string]map[string]bool) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.auto = auto
	idx.rebuildLocked()
}

// known reports whether we have any capability data for a model.
func (idx *capabilityIndex) known(model string) bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	_, ok := idx.caps[strings.TrimSpace(model)]
	return ok
}

// satisfies reports whether a model is KNOWN to support ALL required caps.
// Unknown models return false (fail-closed).
func (idx *capabilityIndex) satisfies(model string, required []string) bool {
	if len(required) == 0 {
		return true
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	set := idx.caps[strings.TrimSpace(model)]
	if set == nil {
		return false
	}
	for _, c := range required {
		if !set[c] {
			return false
		}
	}
	return true
}

// capableModels returns, sorted, every KNOWN model that satisfies all required
// caps — the candidate pool for capability rescue.
func (idx *capabilityIndex) capableModels(required []string) []string {
	idx.mu.RLock()
	models := make([]string, 0)
	for model, set := range idx.caps {
		ok := true
		for _, c := range required {
			if !set[c] {
				ok = false
				break
			}
		}
		if ok {
			models = append(models, model)
		}
	}
	idx.mu.RUnlock()
	sort.Strings(models)
	return models
}

func (idx *capabilityIndex) warnUnknownOnce(model string) {
	if _, loaded := idx.warned.LoadOrStore(model, struct{}{}); !loaded {
		slog.Warn("[CalvoProxy] model has no capability data; treated as incapable for capability-gated requests", slog.String("model", model))
	}
}

// capsRequired maps request signals to the capabilities the model must support.
func capsRequired(hasImages, hasTools bool) []string {
	req := make([]string, 0, 2)
	if hasImages {
		req = append(req, capVision)
	}
	if hasTools {
		req = append(req, capTools)
	}
	return req
}

// --- auto-derive from OpenRouter /models ---

var capabilityHTTPClient = &http.Client{Timeout: 15 * time.Second}

type openRouterModel struct {
	ID           string `json:"id"`
	Architecture struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
	SupportedParameters []string `json:"supported_parameters"`
}

// fetchOpenRouterCapabilities pulls the public model list and derives per-model
// capabilities. Returns a fresh map; on any error returns nil so the caller
// keeps the previous snapshot.
func fetchOpenRouterCapabilities(ctx context.Context) (map[string]map[string]bool, error) {
	url := envValue("PROXY_OPENROUTER_MODELS_URL")
	if url == "" {
		url = "https://openrouter.ai/api/v1/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := capabilityHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, errStatus(resp.StatusCode)
	}
	var payload struct {
		Data []openRouterModel `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	out := make(map[string]map[string]bool, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID == "" {
			continue
		}
		set := map[string]bool{}
		for _, mod := range m.Architecture.InputModalities {
			if strings.EqualFold(mod, "image") {
				set[capVision] = true
			}
		}
		for _, p := range m.SupportedParameters {
			switch {
			case strings.EqualFold(p, "tools"):
				set[capTools] = true
			case strings.EqualFold(p, capReasoning):
				set[capReasoning] = true
			case strings.EqualFold(p, capReasoningEffort):
				set[capReasoningEffort] = true
			}
		}
		out[m.ID] = set
	}
	if len(out) == 0 {
		// A 200 with no models is almost certainly a transient upstream glitch —
		// treat it as an error so the caller keeps the previous snapshot instead
		// of wiping all auto-derived capabilities.
		return nil, errStatus(0)
	}
	return out, nil
}

type errStatus int

func (e errStatus) Error() string { return "openrouter /models HTTP " + itoa(int(e)) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// capabilityAutoderiveEnabled reports whether background auto-derive is on
// (PROXY_CAPABILITY_AUTODERIVE, default true).
func capabilityAutoderiveEnabled() bool { return envBool("PROXY_CAPABILITY_AUTODERIVE", true) }

// startCapabilityRefresh runs the auto-derive fetch once at startup and then on
// an interval, swapping the snapshot atomically. Best-effort: a failed fetch
// keeps the previous (overrides-only) data — offline installs still work.
func (idx *capabilityIndex) startCapabilityRefresh(ctx context.Context) {
	if !capabilityAutoderiveEnabled() {
		return
	}
	go func() {
		refresh := func() {
			fctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			auto, err := fetchOpenRouterCapabilities(fctx)
			if err != nil {
				slog.Debug("capability auto-derive fetch failed; keeping current data", "error", err)
				return
			}
			idx.setAuto(auto)
			slog.Info("[CalvoProxy] model capabilities refreshed", "models", len(auto))
		}
		refresh()
		interval := envDurationSeconds("PROXY_CAPABILITY_REFRESH_SECONDS", 6*time.Hour)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}
