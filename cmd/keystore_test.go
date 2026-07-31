package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestKeystore_StoreLoadDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "openrouter.key")
	t.Setenv("PROXY_KEY_FILE", path)

	if got := storedAPIKey(); got != "" {
		t.Fatalf("expected empty before store, got %q", got)
	}
	if err := storeAPIKey("  sk-or-v1-secret  "); err != nil {
		t.Fatalf("store: %v", err)
	}
	if got := storedAPIKey(); got != "sk-or-v1-secret" {
		t.Fatalf("load: got %q", got)
	}
	// File should exist and (on Unix) be 0600.
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("key file too permissive: %v", info.Mode())
	}
	if err := deleteAPIKey(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := storedAPIKey(); got != "" {
		t.Fatalf("expected empty after delete, got %q", got)
	}
	// Delete again is idempotent.
	if err := deleteAPIKey(); err != nil {
		t.Fatalf("delete idempotent: %v", err)
	}
}

func TestMaskKeyAndShape(t *testing.T) {
	if maskKey("sk-or-v1-abcdefghijklmnop") == "sk-or-v1-abcdefghijklmnop" {
		t.Error("maskKey should not reveal the key")
	}
	if maskKey("short") != "****" {
		t.Error("short key should mask fully")
	}
	if !looksLikeOpenRouterKey("sk-or-v1-abcdefghijkl") {
		t.Error("valid key shape rejected")
	}
	if looksLikeOpenRouterKey("nope") || looksLikeOpenRouterKey("sk-or-") {
		t.Error("bad key shape accepted")
	}
}

// TestResolveAPIKey_FileFallback proves the login-file source is used when the
// header and env are empty (on a loopback bind), and is refused on a public bind
// without opt-in — same ambient-key gate as the env key.
func TestResolveAPIKey_FileFallback(t *testing.T) {
	oldBind := bindHost
	defer func() { bindHost = oldBind }()
	t.Setenv("PROXY_KEY_FILE", filepath.Join(t.TempDir(), "openrouter.key"))
	t.Setenv("OPENROUTER_API_KEY", "") // no env key
	if err := storeAPIKey("sk-or-v1-fromfile-xxxxxxxx"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	// Loopback → file key used.
	bindHost = "127.0.0.1"
	if got := resolveAPIKey(req); got != "sk-or-v1-fromfile-xxxxxxxx" {
		t.Fatalf("loopback should use file key, got %q", got)
	}

	// Public bind, no opt-in → refused (empty).
	bindHost = "0.0.0.0"
	if got := resolveAPIKey(req); got != "" {
		t.Fatalf("public bind should refuse the ambient file key, got %q", got)
	}

	// Public bind + opt-in → allowed.
	t.Setenv("PROXY_ALLOW_ENV_KEY_PUBLIC", "true")
	if got := resolveAPIKey(req); got != "sk-or-v1-fromfile-xxxxxxxx" {
		t.Fatalf("public bind + opt-in should use file key, got %q", got)
	}

	// A real Authorization header always wins and bypasses the ambient gate.
	req.Header.Set("Authorization", "Bearer header-key")
	if got := resolveAPIKey(req); got != "header-key" {
		t.Fatalf("header key should win, got %q", got)
	}
}
