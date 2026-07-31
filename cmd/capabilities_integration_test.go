package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cervantesh/calvoproxy/internal/router"
)

// writeCapPolicy writes a deterministic model policy with capability overrides
// and points PROXY_MODEL_POLICY_FILE at it; auto-derive is disabled so caps come
// only from the file (no network, fully deterministic).
func writeCapPolicy(t *testing.T) {
	t.Helper()
	policy := `{
      "DefaultProfile": "captest",
      "Profiles": {
        "captest": ["model-notools", "model-tools", "model-both"],
        "vision":  ["model-vision"]
      },
      "Aliases": {"captest":"captest","vision":"vision","default":"captest"},
      "Capabilities": {
        "model-notools": [],
        "model-tools":   ["tools"],
        "model-vision":  ["vision"],
        "model-both":    ["vision", "tools"]
      }
    }`
	path := filepath.Join(t.TempDir(), "model-policy.json")
	if err := os.WriteFile(path, []byte(policy), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXY_MODEL_POLICY_FILE", path)
	t.Setenv("PROXY_CAPABILITY_AUTODERIVE", "false")
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	bindHost = "127.0.0.1"
}

// capMockUpstream records the model of each upstream request and returns 200.
func capMockUpstream(t *testing.T, seen *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var b map[string]interface{}
		_ = json.Unmarshal(body, &b)
		mu.Lock()
		*seen = append(*seen, strings.TrimSpace(toStr(b["model"])))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	return srv
}

func toStr(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func TestCapabilityRouting_ToolsAndVisionAndPin(t *testing.T) {
	writeCapPolicy(t)
	var seen []string
	var mu sync.Mutex
	up := capMockUpstream(t, &seen, &mu)
	defer up.Close()
	t.Setenv("PROXY_OPENROUTER_URL", up.URL+"/v1/chat/completions")

	mux := newMux(router.NewRouterService(), nil)

	call := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer dummy")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	lastSeen := func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(seen) == 0 {
			return ""
		}
		return seen[len(seen)-1]
	}

	// 1) Tools request on the captest profile: model-notools is first but lacks
	//    tools, so it must be filtered out and model-tools chosen.
	rec := call("/v1/captest/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools request: got %d: %s", rec.Code, rec.Body.String())
	}
	if got := lastSeen(); got != "model-tools" {
		t.Fatalf("tools request should route to model-tools, upstream saw %q", got)
	}

	// 2) Image request: auto-switches to the vision profile → model-vision.
	img := `{"model":"auto","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	rec = call("/v1/chat/completions", img)
	if rec.Code != http.StatusOK {
		t.Fatalf("image request: got %d: %s", rec.Code, rec.Body.String())
	}
	if got := lastSeen(); got != "model-vision" {
		t.Fatalf("image request should route to model-vision, upstream saw %q", got)
	}

	// 2b) Vision+tools together: vision profile is NOT forced (tools present);
	//     rescue finds a both-capable model (model-both) in the profiles.
	rec = call("/v1/chat/completions", `{"model":"auto","tools":[{"type":"function","function":{"name":"f"}}],"messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("vision+tools request: got %d: %s", rec.Code, rec.Body.String())
	}
	if got := lastSeen(); got != "model-both" {
		t.Fatalf("vision+tools should route to model-both, upstream saw %q", got)
	}

	// 3) Pinning a model that can't do tools → clear 422, no upstream call.
	mu.Lock()
	before := len(seen)
	mu.Unlock()
	rec = call("/v1/captest/chat/completions", `{"model":"model-notools","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("incapable pin should be 422, got %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	after := len(seen)
	mu.Unlock()
	if after != before {
		t.Fatal("incapable pin must not reach the upstream")
	}
}
