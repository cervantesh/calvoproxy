// Package contract verifies what the REAL upstream does, not what a mock says
// it does.
//
// Every mock in this repo encodes an assumption about OpenRouter. When an
// assumption is wrong the unit suite stays green and production breaks — which
// is exactly what happened: one provider rejects requests carrying more than 64
// tools with a 400, no mock modelled that, the chain treated 400 as terminal,
// and a real client lost every turn that reached the capped provider.
//
// These tests spend real quota, so they are OPT-IN:
//
//	CALVOPROXY_CONTRACT=1 OPENROUTER_API_KEY=... go test ./test/contract/ -v
//
// Skipped otherwise, including in the normal CI run. Treat a failure here as a
// change in the upstream contract rather than automatically a bug in this repo —
// the point is to learn it from a test instead of from a user.
package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	chatURL   = "https://openrouter.ai/api/v1/chat/completions"
	modelsURL = "https://openrouter.ai/api/v1/models"
)

func requireOptIn(t *testing.T) string {
	t.Helper()
	if os.Getenv("CALVOPROXY_CONTRACT") == "" {
		t.Skip("contract tests spend real quota; set CALVOPROXY_CONTRACT=1 to run")
	}
	key := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if key == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}
	return key
}

func post(t *testing.T, key string, body map[string]any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, chatURL, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("upstream unreachable: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out
}

func toolPayload(n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = map[string]any{"type": "function", "function": map[string]any{
			"name":        fmt.Sprintf("tool_%d", i),
			"description": "contract probe",
			"parameters": map[string]any{"type": "object",
				"properties": map[string]any{"a": map[string]any{"type": "string"}}},
		}}
	}
	return out
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// The bug that motivated this package. A provider rejects >64 tools with 400.
// The chain only advances past 400 — any other status would terminate it — so
// the exact status is load-bearing, not incidental.
func TestUpstream_ToolCapSurfacesAs400(t *testing.T) {
	key := requireOptIn(t)
	model := envOr("CALVOPROXY_CONTRACT_PICKY_MODEL", "openai/gpt-oss-20b:free")

	code, body := post(t, key, map[string]any{
		"model": model, "max_tokens": 8, "tools": toolPayload(70), "tool_choice": "auto",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	t.Logf("model=%s status=%d body=%s", model, code, truncate(string(body), 300))

	if code == http.StatusOK {
		t.Skipf("%s now accepts 70 tools — the cap this repo routes around may be gone", model)
	}
	if code != http.StatusBadRequest {
		t.Errorf("expected the tool cap to surface as 400, got %d; the chain only "+
			"advances past 400, so another status would terminate the request", code)
	}
	if !strings.Contains(strings.ToLower(string(body)), "tool") {
		t.Errorf("a tool-cap rejection should mention tools, got %s", truncate(string(body), 200))
	}
}

// A model leading a tool-calling chain must accept the payload size real agents
// send. Hermes sends more than 64 tools on every turn.
func TestUpstream_ChainLeaderAcceptsRealisticToolCount(t *testing.T) {
	key := requireOptIn(t)
	model := envOr("CALVOPROXY_CONTRACT_LEAD_MODEL", "nvidia/nemotron-3-super-120b-a12b:free")

	code, body := post(t, key, map[string]any{
		"model": model, "max_tokens": 16, "tools": toolPayload(70), "tool_choice": "auto",
		"messages": []map[string]any{{"role": "user", "content": "say ok"}},
	})
	if code != http.StatusOK {
		t.Fatalf("%s rejected a realistic agent payload (%d): %s — it should not lead "+
			"a tool-calling chain", model, code, truncate(string(body), 300))
	}
}

// The proxy's fail-fast wait counts only `data:` events, because keepalive
// comments arrive precisely while a request is QUEUED — the condition it exists
// to detect. This pins the SSE shape those decisions rest on.
func TestUpstream_StreamShape(t *testing.T) {
	key := requireOptIn(t)
	raw, _ := json.Marshal(map[string]any{
		"model":      envOr("CALVOPROXY_CONTRACT_LEAD_MODEL", "nvidia/nemotron-3-super-120b-a12b:free"),
		"max_tokens": 32, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "count to three"}},
	})
	req, _ := http.NewRequest(http.MethodPost, chatURL, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("upstream unreachable: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Fatalf("stream content-type = %q; the proxy keys its whole streaming path on this", ct)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var dataEvents, comments int
	var sawDone bool
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, ":"):
			comments++
		case strings.HasPrefix(line, "data: [DONE]"):
			sawDone = true
		case strings.HasPrefix(line, "data:"):
			dataEvents++
		}
	}
	t.Logf("data events=%d keepalive comments=%d done=%v", dataEvents, comments, sawDone)

	if dataEvents == 0 {
		t.Error("no data: events — the first-event wait would never be satisfied")
	}
	if !sawDone {
		t.Error("no [DONE] sentinel; streamCopy treats a clean EOF as success and would misreport")
	}
}

// Free slugs get retired. A retired model is a dead chain position that costs a
// fallback step on every request and shows up as nothing in particular.
func TestUpstream_PolicyModelsStillExist(t *testing.T) {
	key := requireOptIn(t)

	req, _ := http.NewRequest(http.MethodGet, modelsURL, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("model catalogue unreachable: %v", err)
	}
	defer resp.Body.Close()

	var catalogue struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalogue); err != nil {
		t.Fatal(err)
	}
	live := make(map[string]bool, len(catalogue.Data))
	for _, m := range catalogue.Data {
		live[m.ID] = true
	}

	policy, err := os.ReadFile("../../model-policy.json")
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Profiles     map[string][]string `json:"Profiles"`
		Capabilities map[string][]string `json:"Capabilities"`
	}
	if err := json.Unmarshal(policy, &p); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for profile, chain := range p.Profiles {
		for _, m := range chain {
			if seen[m] {
				continue
			}
			seen[m] = true
			if !live[m] {
				t.Errorf("%s (profile %q) is gone from the catalogue — a dead chain position", m, profile)
			}
			// The capability filter is fail-closed: an undeclared model cannot
			// serve a tools or vision request at all.
			if _, ok := p.Capabilities[m]; !ok {
				t.Errorf("%s (profile %q) has no declared capabilities; the fail-closed "+
					"filter will never select it for a tools/vision request", m, profile)
			}
		}
	}
	t.Logf("checked %d distinct models across %d profiles", len(seen), len(p.Profiles))
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
