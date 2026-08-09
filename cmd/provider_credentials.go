package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/cervantesh/calvoproxy/internal/secretstore"
)

var managedCredentials secretstore.Store

func vaultFilePath() string {
	if path := strings.TrimSpace(os.Getenv("PROXY_VAULT_FILE")); path != "" {
		return path
	}
	dir, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(dir) == "" {
		return ""
	}
	return filepath.Join(dir, "calvoproxy", "providers.vault")
}

func initializeManagedCredentials(ctx context.Context) secretstore.Store {
	path := vaultFilePath()
	if path == "" {
		slog.Warn("Provider vault unavailable: no safe user configuration directory")
		return nil
	}
	store := secretstore.NewVault(path, secretstore.NewPlatformMasterKeySource(path))
	managedCredentials = store
	snapshot := store.Status(ctx)
	if snapshot.Locked {
		slog.Warn("Provider vault is locked; environment credentials remain available", "backend", snapshot.Backend)
		return store
	}
	migrateLegacyOpenRouterKey(ctx, store)
	return store
}

func migrateLegacyOpenRouterKey(ctx context.Context, store secretstore.Store) {
	legacy := storedAPIKey()
	if legacy == "" {
		return
	}
	legacyBytes := []byte(legacy)
	defer clear(legacyBytes)

	current, exists, err := store.Get(ctx, secretstore.ProviderOpenRouter)
	if err != nil {
		slog.Warn("Could not migrate legacy OpenRouter credential; legacy file remains", "path", keyFilePath(), "error", err)
		return
	}
	if exists {
		matchesLegacy := subtle.ConstantTimeCompare(current, legacyBytes) == 1
		clear(current)
		if !matchesLegacy {
			slog.Warn("Legacy OpenRouter credential differs from the managed vault entry; legacy file remains for manual review", "path", keyFilePath())
			return
		}
	} else {
		if err := store.Set(ctx, secretstore.ProviderOpenRouter, legacyBytes); err != nil {
			slog.Warn("Could not migrate legacy OpenRouter credential; legacy file remains", "path", keyFilePath(), "error", err)
			return
		}
		verified, ok, verifyErr := store.Get(ctx, secretstore.ProviderOpenRouter)
		verifiedMatch := ok && verifyErr == nil && subtle.ConstantTimeCompare(verified, legacyBytes) == 1
		clear(verified)
		if !verifiedMatch {
			slog.Warn("Could not verify migrated OpenRouter credential; legacy file remains", "path", keyFilePath(), "error", verifyErr)
			return
		}
	}
	if err := deleteAPIKey(); err != nil {
		slog.Warn("OpenRouter credential migrated, but the legacy file could not be removed", "path", keyFilePath(), "error", err)
		return
	}
	slog.Info("Migrated legacy OpenRouter credential into the encrypted provider vault", "path", keyFilePath())
}

func managedProviderCredential(provider secretstore.Provider) ([]byte, bool) {
	if managedCredentials == nil {
		return nil, false
	}
	secret, ok, err := managedCredentials.Get(context.Background(), provider)
	if err != nil {
		if !errors.Is(err, secretstore.ErrVaultLocked) {
			slog.Warn("Managed provider credential unavailable", "provider", provider, "error", err)
		}
		return nil, false
	}
	return secret, ok
}

func providerFromName(provider string) (secretstore.Provider, bool) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case string(secretstore.ProviderOpenRouter):
		return secretstore.ProviderOpenRouter, true
	case string(secretstore.ProviderCerebras):
		return secretstore.ProviderCerebras, true
	case string(secretstore.ProviderGroq):
		return secretstore.ProviderGroq, true
	default:
		return "", false
	}
}

func managedProviderCredentialByName(provider string) ([]byte, bool) {
	id, ok := providerFromName(provider)
	if !ok {
		return nil, false
	}
	return managedProviderCredential(id)
}

func managedProviderConfigured(provider secretstore.Provider) bool {
	secret, ok := managedProviderCredential(provider)
	clear(secret)
	return ok
}
