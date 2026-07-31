package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// keyFilePath is where a `calvoproxy login` key is stored. Defaults to
// <user-config-dir>/calvoproxy/openrouter.key (%AppData% on Windows, ~/.config
// on Linux, ~/Library/Application Support on macOS). Override with PROXY_KEY_FILE.
// keyFilePath returns the login-key path, or "" when no safe location can be
// determined (no PROXY_KEY_FILE and no user config dir) — callers treat "" as
// "no key store", so we never fall back to the current working directory.
func keyFilePath() string {
	if p := strings.TrimSpace(os.Getenv("PROXY_KEY_FILE")); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(dir) == "" {
		return "" // fail-closed: don't scatter a secret into the cwd
	}
	return filepath.Join(dir, "calvoproxy", "openrouter.key")
}

// storedAPIKey returns the key saved by `calvoproxy login`, or "" if none.
func storedAPIKey() string {
	path := keyFilePath()
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// storeAPIKey writes the key atomically (temp in the same dir → rename) with
// 0600 perms under a 0700 dir. On Windows the mode bits aren't real ACLs, but the
// path lives under the user profile, which is the security boundary.
func storeAPIKey(key string) error {
	key = strings.TrimSpace(key)
	path := keyFilePath()
	if path == "" {
		return errors.New("cannot determine a config directory to store the key; set PROXY_KEY_FILE")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".openrouter-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_ = tmp.Chmod(0o600)
	if _, err := tmp.WriteString(key + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	// Windows os.Rename won't replace an existing file — remove the target first
	// so a second `login` (without `logout`) doesn't fail after a good exchange.
	_ = os.Remove(path)
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// deleteAPIKey removes the stored key (idempotent).
func deleteAPIKey() error {
	path := keyFilePath()
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// maskKey renders a key for display without revealing it.
func maskKey(k string) string {
	k = strings.TrimSpace(k)
	if len(k) <= 12 {
		return "****"
	}
	return k[:8] + "…" + k[len(k)-4:]
}

// looksLikeOpenRouterKey does a light shape check (never a security control).
func looksLikeOpenRouterKey(k string) bool {
	k = strings.TrimSpace(k)
	return strings.HasPrefix(k, "sk-or-") && len(k) >= 20
}
