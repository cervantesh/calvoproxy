//go:build darwin && cgo

package secretstore

import "testing"

func TestDarwinMasterKeyUsesKeychain(t *testing.T) {
	source := NewPlatformMasterKeySource("/tmp/calvoproxy-vault.json")
	if source.Backend() != "macos-keychain" {
		t.Fatalf("unexpected backend %q", source.Backend())
	}
}
