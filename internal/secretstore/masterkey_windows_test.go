//go:build windows

package secretstore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestWindowsMasterKeyLoadOrCreate(t *testing.T) {
	vaultPath := filepath.Join(t.TempDir(), "secrets.vault")
	source := NewPlatformMasterKeySource(vaultPath)
	first, err := source.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clearBytes(first) })
	if len(first) != masterKeySize {
		t.Fatalf("key length = %d, want %d", len(first), masterKeySize)
	}

	blob, err := os.ReadFile(vaultPath + ".masterkey.dpapi")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, first) {
		t.Fatal("DPAPI blob contains the plaintext master key")
	}

	second, err := NewPlatformMasterKeySource(vaultPath).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clearBytes(second) })
	if !bytes.Equal(first, second) {
		t.Fatal("reloaded master key differs from generated key")
	}
	if source.Backend() != "windows-dpapi-current-user" {
		t.Fatalf("unexpected backend %q", source.Backend())
	}
}

func TestWindowsMasterKeyConcurrentLoadUsesOneKey(t *testing.T) {
	vaultPath := filepath.Join(t.TempDir(), "secrets.vault")
	const workers = 12
	keys := make([][]byte, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			keys[index], errs[index] = NewPlatformMasterKeySource(vaultPath).Load(context.Background())
		}(i)
	}
	wg.Wait()
	for i := range keys {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if !bytes.Equal(keys[0], keys[i]) {
			t.Fatalf("worker %d loaded a different key", i)
		}
	}
	for i := range keys {
		clearBytes(keys[i])
	}
}

func TestWindowsMasterKeyRejectsCorruptBlob(t *testing.T) {
	vaultPath := filepath.Join(t.TempDir(), "secrets.vault")
	if err := os.WriteFile(vaultPath+".masterkey.dpapi", []byte("not-dpapi"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewPlatformMasterKeySource(vaultPath).Load(context.Background())
	if err == nil || !strings.Contains(err.Error(), "CryptUnprotectData") {
		t.Fatalf("expected DPAPI error, got %v", err)
	}
}

func TestWindowsMasterKeyHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewPlatformMasterKeySource(filepath.Join(t.TempDir(), "vault")).Load(ctx)
	if err != context.Canceled {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
