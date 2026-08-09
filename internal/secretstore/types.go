package secretstore

import (
	"context"
	"errors"
)

// Provider is a provider whose single managed credential may be stored in the
// vault. Keep this allowlist deliberately small: arbitrary keys must never turn
// the vault into an unstructured secret database.
type Provider string

const (
	ProviderOpenRouter Provider = "openrouter"
	ProviderCerebras   Provider = "cerebras"
	ProviderGroq       Provider = "groq"
)

var (
	ErrInvalidProvider = errors.New("secretstore: invalid provider")
	ErrInvalidSecret   = errors.New("secretstore: invalid secret")
	ErrInvalidKey      = errors.New("secretstore: master key must be exactly 32 bytes")
	ErrVaultLocked     = errors.New("secretstore: vault is locked")
)

// MasterKeySource retrieves the 256-bit key protected by the host OS.
type MasterKeySource interface {
	Load(context.Context) ([]byte, error)
	Backend() string
}

// Store holds at most one managed credential for each allowed provider.
type Store interface {
	Get(context.Context, Provider) ([]byte, bool, error)
	Set(context.Context, Provider, []byte) error
	Delete(context.Context, Provider) error
	Status(context.Context) Snapshot
}

type ProviderStatus struct {
	Provider   Provider `json:"provider"`
	Configured bool     `json:"configured"`
}

// Snapshot intentionally contains no secret-derived metadata (including a
// suffix or hash) that could become an oracle through an admin endpoint.
type Snapshot struct {
	Backend   string           `json:"backend"`
	Available bool             `json:"available"`
	Locked    bool             `json:"locked"`
	Providers []ProviderStatus `json:"providers"`
}

func allowedProvider(provider Provider) bool {
	switch provider {
	case ProviderOpenRouter, ProviderCerebras, ProviderGroq:
		return true
	default:
		return false
	}
}

var allProviders = [...]Provider{ProviderOpenRouter, ProviderCerebras, ProviderGroq}
