package secretstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

type testKeySource struct {
	key     []byte
	err     error
	backend string
}

func (s testKeySource) Load(context.Context) ([]byte, error) {
	return append([]byte(nil), s.key...), s.err
}
func (s testKeySource) Backend() string { return s.backend }

func testVault(t *testing.T) (*Vault, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.vault")
	return NewVault(path, testKeySource{key: bytes.Repeat([]byte{0x42}, 32), backend: "test"}), path
}

func TestVaultRoundTripAndStatus(t *testing.T) {
	vault, path := testVault(t)
	ctx := context.Background()
	if err := vault.Set(ctx, ProviderOpenRouter, []byte("  secret-openrouter  \t")); err != nil {
		t.Fatal(err)
	}
	got, ok, err := vault.Get(ctx, ProviderOpenRouter)
	if err != nil || !ok || string(got) != "secret-openrouter" {
		t.Fatalf("Get() = %q, %v, %v", got, ok, err)
	}
	zero(got)
	if _, ok, err := vault.Get(ctx, ProviderGroq); err != nil || ok {
		t.Fatalf("missing Get() = _, %v, %v", ok, err)
	}
	snapshot := vault.Status(ctx)
	if snapshot.Backend != "test" || !snapshot.Available || snapshot.Locked {
		t.Fatalf("unexpected status: %+v", snapshot)
	}
	if !configured(snapshot, ProviderOpenRouter) || configured(snapshot, ProviderGroq) {
		t.Fatalf("unexpected providers: %+v", snapshot.Providers)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("vault mode = %o", info.Mode().Perm())
	}
	if err := vault.Delete(ctx, ProviderOpenRouter); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := vault.Get(ctx, ProviderOpenRouter); err != nil || ok {
		t.Fatalf("deleted Get() = _, %v, %v", ok, err)
	}
}

func configured(snapshot Snapshot, provider Provider) bool {
	for _, status := range snapshot.Providers {
		if status.Provider == provider {
			return status.Configured
		}
	}
	return false
}

func TestVaultProviderAllowlist(t *testing.T) {
	vault, _ := testVault(t)
	ctx := context.Background()
	for _, provider := range []Provider{"OpenRouter", " openrouter", "custom", ""} {
		if err := vault.Set(ctx, provider, []byte("secret")); !errors.Is(err, ErrInvalidProvider) {
			t.Fatalf("Set(%q) error = %v", provider, err)
		}
		if _, _, err := vault.Get(ctx, provider); !errors.Is(err, ErrInvalidProvider) {
			t.Fatalf("Get(%q) error = %v", provider, err)
		}
		if err := vault.Delete(ctx, provider); !errors.Is(err, ErrInvalidProvider) {
			t.Fatalf("Delete(%q) error = %v", provider, err)
		}
	}
}

func TestVaultRejectsInvalidSecrets(t *testing.T) {
	vault, _ := testVault(t)
	cases := [][]byte{
		nil,
		{},
		[]byte(" \t "),
		[]byte("line\nbreak"),
		[]byte("carriage\rreturn"),
		[]byte{'n', 0, 'l'},
		{0xff, 0xfe},
		bytes.Repeat([]byte("x"), maxSecretLen+1),
	}
	for _, secret := range cases {
		if err := vault.Set(context.Background(), ProviderGroq, secret); !errors.Is(err, ErrInvalidSecret) {
			t.Fatalf("Set(%q) error = %v", secret, err)
		}
	}
	if err := vault.Set(context.Background(), ProviderGroq, bytes.Repeat([]byte("x"), maxSecretLen)); err != nil {
		t.Fatalf("maximum-length secret rejected: %v", err)
	}
}

func TestVaultFileContainsNoPlaintextOrSecretSuffix(t *testing.T) {
	vault, path := testVault(t)
	secret := []byte("sentinel-prefix-ULTRA-SECRET-SUFFIX-9f2a")
	if err := vault.Set(context.Background(), ProviderCerebras, secret); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range [][]byte{secret, []byte("ULTRA-SECRET"), []byte("9f2a")} {
		if bytes.Contains(data, sentinel) {
			t.Fatalf("vault contains plaintext sentinel %q: %s", sentinel, data)
		}
	}
}

func TestVaultReplacementUsesFreshNonce(t *testing.T) {
	vault, path := testVault(t)
	vault.random = bytes.NewReader(append(bytes.Repeat([]byte{1}, 12), bytes.Repeat([]byte{2}, 12)...))
	if err := vault.Set(context.Background(), ProviderGroq, []byte("first")); err != nil {
		t.Fatal(err)
	}
	first := readTestFile(t, path).Entries[ProviderGroq].Nonce
	if err := vault.Set(context.Background(), ProviderGroq, []byte("second")); err != nil {
		t.Fatal(err)
	}
	second := readTestFile(t, path).Entries[ProviderGroq].Nonce
	if bytes.Equal(first, second) || !bytes.Equal(first, bytes.Repeat([]byte{1}, 12)) || !bytes.Equal(second, bytes.Repeat([]byte{2}, 12)) {
		t.Fatalf("nonces not fresh/injected: %x %x", first, second)
	}
}

func TestVaultFailedWritePreservesLastValidFile(t *testing.T) {
	vault, path := testVault(t)
	ctx := context.Background()
	if err := vault.Set(ctx, ProviderOpenRouter, []byte("old-secret")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	vault.replace = func(string, string) error { return errors.New("injected replace failure") }
	if err := vault.Set(ctx, ProviderOpenRouter, []byte("new-secret")); err == nil {
		t.Fatal("Set succeeded despite injected failure")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed replacement changed the last valid vault")
	}
	got, ok, err := vault.Get(ctx, ProviderOpenRouter)
	if err != nil || !ok || string(got) != "old-secret" {
		t.Fatalf("Get after failure = %q, %v, %v", got, ok, err)
	}
}

func TestVaultEntropyFailurePreservesLastValidFile(t *testing.T) {
	vault, path := testVault(t)
	ctx := context.Background()
	if err := vault.Set(ctx, ProviderOpenRouter, []byte("old-secret")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	vault.random = errReader{}
	if err := vault.Set(ctx, ProviderOpenRouter, []byte("new-secret")); err == nil {
		t.Fatal("Set succeeded despite entropy failure")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("entropy failure changed the vault")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("injected entropy failure") }

func TestVaultFailsLockedWithoutRepairingCorruption(t *testing.T) {
	ctx := context.Background()
	tests := map[string]func(*testing.T, *Vault, string){
		"wrong-key": func(t *testing.T, vault *Vault, _ string) {
			vault.keys = testKeySource{key: bytes.Repeat([]byte{0x99}, 32), backend: "test"}
		},
		"tamper": func(t *testing.T, _ *Vault, path string) {
			file := readTestFile(t, path)
			file.Entries[ProviderOpenRouter].Ciphertext[0] ^= 0xff
			writeTestFile(t, path, file)
		},
		"truncate": func(t *testing.T, _ *Vault, path string) {
			data, _ := os.ReadFile(path)
			if err := os.WriteFile(path, data[:len(data)/2], 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"unknown-version": func(t *testing.T, _ *Vault, path string) {
			file := readTestFile(t, path)
			file.Version = 2
			writeTestFile(t, path, file)
		},
	}
	for name, corrupt := range tests {
		t.Run(name, func(t *testing.T) {
			vault, path := testVault(t)
			if err := vault.Set(ctx, ProviderOpenRouter, []byte("known-good")); err != nil {
				t.Fatal(err)
			}
			corrupt(t, vault, path)
			before, _ := os.ReadFile(path)
			if _, _, err := vault.Get(ctx, ProviderOpenRouter); !errors.Is(err, ErrVaultLocked) {
				t.Fatalf("Get error = %v", err)
			}
			if err := vault.Set(ctx, ProviderGroq, []byte("must-not-write")); !errors.Is(err, ErrVaultLocked) {
				t.Fatalf("Set error = %v", err)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("locked operation repaired or rewrote the vault")
			}
			if status := vault.Status(ctx); !status.Locked || status.Available {
				t.Fatalf("unexpected status: %+v", status)
			}
		})
	}
}

func TestGetDecryptsOnlyRequestedProvider(t *testing.T) {
	vault, path := testVault(t)
	ctx := context.Background()
	if err := vault.Set(ctx, ProviderOpenRouter, []byte("openrouter-secret")); err != nil {
		t.Fatal(err)
	}
	if err := vault.Set(ctx, ProviderGroq, []byte("groq-secret")); err != nil {
		t.Fatal(err)
	}
	file := readTestFile(t, path)
	file.Entries[ProviderGroq].Ciphertext[0] ^= 1
	writeTestFile(t, path, file)
	got, ok, err := vault.Get(ctx, ProviderOpenRouter)
	if err != nil || !ok || string(got) != "openrouter-secret" {
		t.Fatalf("independent Get = %q, %v, %v", got, ok, err)
	}
	if err := vault.Set(ctx, ProviderCerebras, []byte("new")); !errors.Is(err, ErrVaultLocked) {
		t.Fatalf("mutation did not detect unrelated corruption: %v", err)
	}
}

func TestVaultConcurrentAccess(t *testing.T) {
	vault, _ := testVault(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			provider := allProviders[i%len(allProviders)]
			secret := []byte(fmt.Sprintf("secret-%d", i))
			if err := vault.Set(ctx, provider, secret); err != nil {
				t.Errorf("Set: %v", err)
				return
			}
			got, ok, err := vault.Get(ctx, provider)
			if err != nil || !ok || len(got) == 0 {
				t.Errorf("Get = %q, %v, %v", got, ok, err)
			}
			zero(got)
		}()
	}
	wg.Wait()
	if status := vault.Status(ctx); status.Locked || !status.Available {
		t.Fatalf("final status: %+v", status)
	}
}

func TestVaultInvalidMasterKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault")
	vault := NewVault(path, testKeySource{key: []byte("short"), backend: "test"})
	if err := vault.Set(context.Background(), ProviderGroq, []byte("secret")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Set error = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("vault unexpectedly created: %v", err)
	}
}

func readTestFile(t *testing.T, path string) vaultFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file vaultFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

func writeTestFile(t *testing.T, path string, file vaultFile) {
	t.Helper()
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestVaultReplacementIncrementsRevisionAndRefreshesTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.json")
	store := NewVault(path, testKeySource{key: bytes.Repeat([]byte{0x42}, 32), backend: "test"})
	if err := store.Set(context.Background(), ProviderGroq, []byte("first-secret")); err != nil {
		t.Fatal(err)
	}
	first := readTestFile(t, path).Entries[ProviderGroq]
	if first.Revision != 1 || first.UpdatedAt == "" {
		t.Fatalf("initial metadata = revision %d, updated_at %q", first.Revision, first.UpdatedAt)
	}
	time.Sleep(time.Millisecond)
	if err := store.Set(context.Background(), ProviderGroq, []byte("second-secret")); err != nil {
		t.Fatal(err)
	}
	second := readTestFile(t, path).Entries[ProviderGroq]
	if second.Revision != 2 || second.UpdatedAt <= first.UpdatedAt {
		t.Fatalf("replacement metadata did not advance: first=%+v second=%+v", first, second)
	}
}
