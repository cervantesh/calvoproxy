package secretstore

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	vaultVersion = 1
	maxSecretLen = 4096
)

type encryptedEntry struct {
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
	Revision   uint64 `json:"revision"`
	UpdatedAt  string `json:"updated_at"`
}

type vaultFile struct {
	Version int                         `json:"version"`
	Entries map[Provider]encryptedEntry `json:"entries"`
}

type Vault struct {
	mu      sync.Mutex
	path    string
	keys    MasterKeySource
	random  io.Reader
	replace func(string, string) error
}

func NewVault(path string, keys MasterKeySource) *Vault {
	return &Vault{path: path, keys: keys, random: rand.Reader, replace: atomicReplaceFile}
}

func (v *Vault) Get(ctx context.Context, provider Provider) ([]byte, bool, error) {
	if !allowedProvider(provider) {
		return nil, false, ErrInvalidProvider
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	file, exists, err := v.readFile()
	if err != nil {
		return nil, false, locked(err)
	}
	if !exists {
		return nil, false, nil
	}
	entry, ok := file.Entries[provider]
	if !ok {
		return nil, false, nil
	}
	key, err := v.loadKey(ctx)
	if err != nil {
		return nil, false, err
	}
	defer zero(key)
	plaintext, err := decrypt(key, provider, entry)
	if err != nil {
		return nil, false, locked(err)
	}
	return plaintext, true, nil
}

func (v *Vault) Set(ctx context.Context, provider Provider, secret []byte) error {
	if !allowedProvider(provider) {
		return ErrInvalidProvider
	}
	clean, err := validateSecret(secret)
	if err != nil {
		return err
	}
	defer zero(clean)

	v.mu.Lock()
	defer v.mu.Unlock()
	file, _, err := v.readFile()
	if err != nil {
		return locked(err)
	}
	key, err := v.loadKey(ctx)
	if err != nil {
		return err
	}
	defer zero(key)
	if err := authenticateEntries(key, file.Entries); err != nil {
		return locked(err)
	}
	entry, err := v.encrypt(key, provider, clean)
	if err != nil {
		return err
	}
	entry.Revision = file.Entries[provider].Revision + 1
	entry.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	file.Entries[provider] = entry
	return v.writeFile(file)
}

func (v *Vault) Delete(ctx context.Context, provider Provider) error {
	if !allowedProvider(provider) {
		return ErrInvalidProvider
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	file, exists, err := v.readFile()
	if err != nil {
		return locked(err)
	}
	if !exists {
		return nil
	}
	// Authenticate the vault before modifying it. A wrong key must never be
	// allowed to replace an otherwise valid file.
	key, err := v.loadKey(ctx)
	if err != nil {
		return err
	}
	defer zero(key)
	if err := authenticateEntries(key, file.Entries); err != nil {
		return locked(err)
	}
	if _, ok := file.Entries[provider]; !ok {
		return nil
	}
	delete(file.Entries, provider)
	return v.writeFile(file)
}

func (v *Vault) Status(ctx context.Context) Snapshot {
	snapshot := Snapshot{Backend: v.keys.Backend(), Providers: providerStatuses(nil)}
	v.mu.Lock()
	defer v.mu.Unlock()
	file, exists, err := v.readFile()
	if err != nil {
		snapshot.Locked = true
		return snapshot
	}
	if !exists {
		key, keyErr := v.loadKey(ctx)
		zero(key)
		snapshot.Available = keyErr == nil
		snapshot.Locked = keyErr != nil
		return snapshot
	}
	key, err := v.loadKey(ctx)
	if err != nil {
		snapshot.Locked = true
		return snapshot
	}
	defer zero(key)
	for provider, entry := range file.Entries {
		plaintext, decryptErr := decrypt(key, provider, entry)
		zero(plaintext)
		if decryptErr != nil {
			snapshot.Locked = true
			return snapshot
		}
	}
	snapshot.Available = true
	snapshot.Providers = providerStatuses(file.Entries)
	return snapshot
}

func providerStatuses(entries map[Provider]encryptedEntry) []ProviderStatus {
	statuses := make([]ProviderStatus, 0, len(allProviders))
	for _, provider := range allProviders {
		_, configured := entries[provider]
		statuses = append(statuses, ProviderStatus{Provider: provider, Configured: configured})
	}
	return statuses
}

func validateSecret(secret []byte) ([]byte, error) {
	clean := append([]byte(nil), bytes.TrimSpace(secret)...)
	if len(clean) == 0 || len(clean) > maxSecretLen || !utf8.Valid(clean) {
		zero(clean)
		return nil, ErrInvalidSecret
	}
	for remaining := clean; len(remaining) > 0; {
		r, size := utf8.DecodeRune(remaining)
		if unicode.IsControl(r) {
			zero(clean)
			return nil, ErrInvalidSecret
		}
		remaining = remaining[size:]
	}
	return clean, nil
}

func (v *Vault) loadKey(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if v.keys == nil {
		return nil, ErrVaultLocked
	}
	loaded, err := v.keys.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: master key unavailable", ErrVaultLocked)
	}
	if len(loaded) != 32 {
		zero(loaded)
		return nil, ErrInvalidKey
	}
	key := append([]byte(nil), loaded...)
	zero(loaded)
	return key, nil
}

func (v *Vault) encrypt(key []byte, provider Provider, plaintext []byte) (encryptedEntry, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return encryptedEntry{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(v.random, nonce); err != nil {
		return encryptedEntry{}, err
	}
	ciphertext := aead.Seal(nil, nonce, plaintext, additionalData(provider))
	return encryptedEntry{Nonce: nonce, Ciphertext: ciphertext}, nil
}

func decrypt(key []byte, provider Provider, entry encryptedEntry) ([]byte, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	if len(entry.Nonce) != aead.NonceSize() || len(entry.Ciphertext) < aead.Overhead() {
		return nil, errors.New("invalid encrypted entry")
	}
	return aead.Open(nil, entry.Nonce, entry.Ciphertext, additionalData(provider))
}

func authenticateEntries(key []byte, entries map[Provider]encryptedEntry) error {
	for provider, entry := range entries {
		plaintext, err := decrypt(key, provider, entry)
		zero(plaintext)
		if err != nil {
			return err
		}
	}
	return nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func additionalData(provider Provider) []byte {
	return []byte(fmt.Sprintf("version=%d;provider=%s", vaultVersion, provider))
}

func (v *Vault) readFile() (vaultFile, bool, error) {
	file := vaultFile{Version: vaultVersion, Entries: make(map[Provider]encryptedEntry)}
	data, err := os.ReadFile(v.path)
	if errors.Is(err, os.ErrNotExist) {
		return file, false, nil
	}
	if err != nil {
		return file, false, err
	}
	if len(data) == 0 || json.Unmarshal(data, &file) != nil {
		return vaultFile{}, true, errors.New("invalid vault file")
	}
	if file.Version != vaultVersion || file.Entries == nil {
		return vaultFile{}, true, errors.New("unsupported or invalid vault version")
	}
	for provider, entry := range file.Entries {
		if !allowedProvider(provider) {
			return vaultFile{}, true, errors.New("vault contains an unknown provider")
		}
		if entry.Revision == 0 {
			return vaultFile{}, true, errors.New("vault entry has an invalid revision")
		}
		if _, err := time.Parse(time.RFC3339Nano, entry.UpdatedAt); err != nil {
			return vaultFile{}, true, errors.New("vault entry has an invalid update time")
		}
	}
	return file, true, nil
}

func (v *Vault) writeFile(file vaultFile) error {
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	dir := filepath.Dir(v.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".calvoproxy-vault-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := v.replace(tmpName, v.path); err != nil {
		return err
	}
	if err := syncParentDir(dir); err != nil {
		return err
	}
	ok = true
	return nil
}

func locked(err error) error {
	return fmt.Errorf("%w: %v", ErrVaultLocked, err)
}

// zero performs best-effort clearing of mutable secret buffers. Go's runtime
// and standard HTTP stack can still create copies, so this is defense in depth.
func zero(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
