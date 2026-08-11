package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cervantesh/calvoproxy/internal/secretstore"
)

type memoryCredentialStore struct {
	values map[secretstore.Provider][]byte
	err    error
}

func (s *memoryCredentialStore) Get(_ context.Context, provider secretstore.Provider) ([]byte, bool, error) {
	if s.err != nil {
		return nil, false, s.err
	}
	value, ok := s.values[provider]
	return append([]byte(nil), value...), ok, nil
}

func (s *memoryCredentialStore) Set(_ context.Context, provider secretstore.Provider, secret []byte) error {
	if s.err != nil {
		return s.err
	}
	if s.values == nil {
		s.values = make(map[secretstore.Provider][]byte)
	}
	s.values[provider] = append([]byte(nil), secret...)
	return nil
}

func (s *memoryCredentialStore) Delete(_ context.Context, provider secretstore.Provider) error {
	delete(s.values, provider)
	return nil
}

func (s *memoryCredentialStore) Status(context.Context) secretstore.Snapshot {
	return secretstore.Snapshot{Available: s.err == nil, Locked: errors.Is(s.err, secretstore.ErrVaultLocked)}
}

func useManagedCredentialStore(t *testing.T, store secretstore.Store) {
	t.Helper()
	previous := managedCredentials
	managedCredentials = store
	t.Cleanup(func() { managedCredentials = previous })
}

func credentialRequest(t *testing.T, authorization string) *http.Request {
	t.Helper()
	req := httptest.NewRequest("POST", "http://localhost/v1/chat/completions", nil)
	if authorization != "" {
		req.Header.Set("Authorization", "Bearer "+authorization)
	}
	return req
}

func TestResolveAPIKeyManagedVaultPrecedence(t *testing.T) {
	store := &memoryCredentialStore{values: map[secretstore.Provider][]byte{secretstore.ProviderOpenRouter: []byte("vault-key")}}
	useManagedCredentialStore(t, store)
	t.Setenv("HOST", "127.0.0.1")
	oldBindHost := bindHost
	bindHost = "127.0.0.1"
	t.Cleanup(func() { bindHost = oldBindHost })
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("PROXY_KEY_FILE", filepath.Join(t.TempDir(), "missing.key"))

	req := credentialRequest(t, "")
	if got := resolveAPIKey(req); got != "vault-key" {
		t.Fatalf("resolveAPIKey() = %q, want vault key", got)
	}
	t.Setenv("OPENROUTER_API_KEY", "environment-key")
	if got := resolveAPIKey(req); got != "environment-key" {
		t.Fatalf("environment must override vault, got %q", got)
	}
	req = credentialRequest(t, "request-key")
	if got := resolveAPIKey(req); got != "request-key" {
		t.Fatalf("request must override environment and vault, got %q", got)
	}
}

func TestManagedVaultCredentialsRefusedOnPublicBind(t *testing.T) {
	store := &memoryCredentialStore{values: map[secretstore.Provider][]byte{
		secretstore.ProviderOpenRouter: []byte("vault-openrouter"),
		secretstore.ProviderCerebras:   []byte("vault-cerebras"),
	}}
	useManagedCredentialStore(t, store)
	t.Setenv("HOST", "0.0.0.0")
	t.Setenv("PROXY_ALLOW_ENV_KEY_PUBLIC", "")
	oldBindHost := bindHost
	bindHost = "0.0.0.0"
	t.Cleanup(func() { bindHost = oldBindHost })

	if got := resolveAPIKey(credentialRequest(t, "")); got != "" {
		t.Fatalf("managed OpenRouter key exposed on public bind: %q", got)
	}
	if ambientDirectProviderConfigured() {
		t.Fatal("managed direct-provider key exposed on public bind")
	}
}

func TestMigrateLegacyOpenRouterCredential(t *testing.T) {
	legacyPath := filepath.Join(t.TempDir(), "openrouter.key")
	t.Setenv("PROXY_KEY_FILE", legacyPath)
	if err := os.WriteFile(legacyPath, []byte("legacy-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &memoryCredentialStore{values: make(map[secretstore.Provider][]byte)}
	migrateLegacyOpenRouterKey(context.Background(), store)

	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy file must be removed after verified migration: %v", err)
	}
	got := string(store.values[secretstore.ProviderOpenRouter])
	if got != "legacy-secret" {
		t.Fatalf("migrated credential = %q", got)
	}
}

func TestMigrationFailurePreservesLegacyFile(t *testing.T) {
	legacyPath := filepath.Join(t.TempDir(), "openrouter.key")
	t.Setenv("PROXY_KEY_FILE", legacyPath)
	if err := os.WriteFile(legacyPath, []byte("legacy-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	migrateLegacyOpenRouterKey(context.Background(), &memoryCredentialStore{err: secretstore.ErrVaultLocked})
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("failed migration removed legacy file: %v", err)
	}
}

func TestMigrationPreservesDifferingLegacyCredential(t *testing.T) {
	legacyPath := filepath.Join(t.TempDir(), "openrouter.key")
	t.Setenv("PROXY_KEY_FILE", legacyPath)
	if err := os.WriteFile(legacyPath, []byte("legacy-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &memoryCredentialStore{values: map[secretstore.Provider][]byte{
		secretstore.ProviderOpenRouter: []byte("different-managed-secret"),
	}}
	migrateLegacyOpenRouterKey(context.Background(), store)
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("differing legacy credential should remain for manual review: %v", err)
	}
	if got := string(store.values[secretstore.ProviderOpenRouter]); got != "different-managed-secret" {
		t.Fatalf("migration overwrote managed credential: %q", got)
	}
}
