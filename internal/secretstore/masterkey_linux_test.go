//go:build linux

package secretstore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxMasterKeyLoadsSystemdCredential(t *testing.T) {
	directory := t.TempDir()
	key := bytes.Repeat([]byte{0x42}, masterKeySize)
	writeLinuxTestKey(t, filepath.Join(directory, systemdMasterKeyCredential), key, 0o400)
	t.Setenv("CREDENTIALS_DIRECTORY", directory)
	t.Setenv(masterKeyFileEnvironment, "")

	source := NewPlatformMasterKeySource("ignored")
	got, err := source.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("loaded key differs")
	}
	if source.Backend() != "linux-systemd-credential" {
		t.Fatalf("unexpected backend %q", source.Backend())
	}
}

func TestLinuxMasterKeyLoadsExplicitFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	key := bytes.Repeat([]byte{0x24}, masterKeySize)
	writeLinuxTestKey(t, path, key, 0o600)
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	t.Setenv(masterKeyFileEnvironment, path)

	got, err := NewPlatformMasterKeySource("ignored").Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("loaded key differs")
	}
}

func TestLinuxMasterKeyFailsClosed(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	t.Setenv(masterKeyFileEnvironment, "")
	_, err := NewPlatformMasterKeySource("ignored").Load(context.Background())
	if err == nil || !strings.Contains(err.Error(), "automatic generation is disabled") {
		t.Fatalf("expected fail-closed error, got %v", err)
	}
}

func TestLinuxMasterKeyRejectsUnsafeInputs(t *testing.T) {
	t.Run("wrong length", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "key")
		writeLinuxTestKey(t, path, []byte("short"), 0o600)
		t.Setenv("CREDENTIALS_DIRECTORY", "")
		t.Setenv(masterKeyFileEnvironment, path)
		_, err := NewPlatformMasterKeySource("ignored").Load(context.Background())
		if err == nil || !strings.Contains(err.Error(), "exactly 32") {
			t.Fatalf("expected length error, got %v", err)
		}
	})

	t.Run("group readable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "key")
		writeLinuxTestKey(t, path, bytes.Repeat([]byte{1}, masterKeySize), 0o640)
		t.Setenv("CREDENTIALS_DIRECTORY", "")
		t.Setenv(masterKeyFileEnvironment, path)
		_, err := NewPlatformMasterKeySource("ignored").Load(context.Background())
		if err == nil || !strings.Contains(err.Error(), "group or world") {
			t.Fatalf("expected permissions error, got %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		directory := t.TempDir()
		target := filepath.Join(directory, "target")
		writeLinuxTestKey(t, target, bytes.Repeat([]byte{1}, masterKeySize), 0o600)
		link := filepath.Join(directory, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CREDENTIALS_DIRECTORY", "")
		t.Setenv(masterKeyFileEnvironment, link)
		_, err := NewPlatformMasterKeySource("ignored").Load(context.Background())
		if err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("expected symlink error, got %v", err)
		}
	})
}

func writeLinuxTestKey(t *testing.T, path string, key []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, key, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
