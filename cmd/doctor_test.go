package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestYAMLScalarHelpers(t *testing.T) {
	lines := strings.Split(`# comment
model:
  provider: custom
  base_url: http://127.0.0.1:8080/v1     # trailing comment
  default: coding
custom_providers:
  - name: calvoproxy
    discover_models: false
discover_models: true
`, "\n")

	if got := yamlNestedScalar(lines, "model", "provider"); got != "custom" {
		t.Errorf("model.provider = %q, want custom", got)
	}
	if got := yamlNestedScalar(lines, "model", "base_url"); got != "http://127.0.0.1:8080/v1" {
		t.Errorf("model.base_url = %q (trailing comment not stripped?)", got)
	}
	// A nested key must not be mistaken for a top-level one.
	if got := yamlNestedScalar(lines, "model", "name"); got != "" {
		t.Errorf("model.name = %q, want empty", got)
	}
	if !yamlHasTopLevelKey(lines, "custom_providers") {
		t.Error("custom_providers should be detected as top-level")
	}
	if yamlHasTopLevelKey(lines, "provider") {
		t.Error("indented `provider:` must not count as top-level")
	}
	if got := yamlScalar(lines, "discover_models"); got != "true" {
		t.Errorf("top-level discover_models = %q, want true", got)
	}
}

func TestStripYAMLCommentKeepsFragment(t *testing.T) {
	// A '#' not preceded by whitespace is part of the value, not a comment.
	if got := stripYAMLComment("http://host/path#frag"); got != "http://host/path#frag" {
		t.Errorf("fragment was stripped: %q", got)
	}
	if got := strings.TrimSpace(stripYAMLComment("value  # note")); got != "value" {
		t.Errorf("comment not stripped: %q", got)
	}
}

// A config missing model.base_url is the exact failure that silently routes to
// OpenRouter, so doctor must report it as FAIL, not a warning.
func TestCheckHermesFlagsMissingBaseURL(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("model:\n  provider: custom\n  default: coding\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERMES_HOME", dir)

	var found bool
	for _, r := range checkHermes("http://127.0.0.1:8080") {
		if strings.Contains(r.title, "model.base_url") {
			found = true
			if r.status != statusFail {
				t.Errorf("missing model.base_url should FAIL, got %v", r.status.label())
			}
		}
	}
	if !found {
		t.Fatal("no model.base_url check was produced")
	}
}

// Regression: Windows tooling writes config.yaml with a UTF-8 BOM, which hid
// the first top-level key and made doctor report a bogus "model.provider" FAIL
// against a perfectly valid config.
func TestCheckHermesHandlesUTF8BOM(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	body := append([]byte{0xEF, 0xBB, 0xBF}, []byte(hermesConfigBlock("http://127.0.0.1:8080/v1"))...)
	if err := os.WriteFile(cfg, body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERMES_HOME", dir)

	for _, r := range checkHermes("http://127.0.0.1:8080") {
		if r.status == statusFail {
			t.Errorf("BOM must not break parsing: %s — %s", r.title, r.detail)
		}
	}
}

func TestCheckHermesAcceptsCorrectConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	body := hermesConfigBlock("http://127.0.0.1:8080/v1")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERMES_HOME", dir)

	for _, r := range checkHermes("http://127.0.0.1:8080") {
		if r.status == statusFail {
			t.Errorf("the block doctor prints must itself pass: %s — %s", r.title, r.detail)
		}
	}
}

// A hostname base_url resolves to ::1 on some hosts while the proxy listens on
// IPv4 only — warn rather than pass silently.
func TestCheckHermesWarnsOnLocalhostHostname(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	body := "model:\n  provider: custom\n  base_url: http://localhost:8080/v1\ncustom_providers:\n  - base_url: http://localhost:8080/v1\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERMES_HOME", dir)

	for _, r := range checkHermes("http://127.0.0.1:8080") {
		if strings.Contains(r.title, "model.base_url") && r.status != statusWarn {
			t.Errorf("localhost hostname should WARN, got %v", r.status.label())
		}
	}
}

func TestCheckRoundTripReportsUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()

	got := checkRoundTrip(srv.Client(), srv.URL, "simple")
	if got.status != statusFail {
		t.Fatalf("HTTP 429 should FAIL, got %v", got.status.label())
	}
	if !strings.Contains(got.detail, "429") {
		t.Errorf("detail should surface the status code, got %q", got.detail)
	}
}

func TestCheckRoundTripSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "vendor/model:free",
			"choices": []any{map[string]any{"message": map[string]any{"content": "pong"}}},
		})
	}))
	defer srv.Close()

	got := checkRoundTrip(srv.Client(), srv.URL, "coding")
	if got.status != statusOK {
		t.Fatalf("healthy upstream should pass, got %v (%s)", got.status.label(), got.detail)
	}
	if !strings.Contains(got.detail, "vendor/model:free") {
		t.Errorf("detail should name the model actually used, got %q", got.detail)
	}
}

func TestProxyBaseURLUsesLoopbackForWildcardBind(t *testing.T) {
	t.Setenv("HOST", "0.0.0.0")
	t.Setenv("PORT", "9999")
	if got := proxyBaseURL(); got != "http://127.0.0.1:9999" {
		t.Errorf("wildcard bind should be probed over loopback, got %q", got)
	}
}
