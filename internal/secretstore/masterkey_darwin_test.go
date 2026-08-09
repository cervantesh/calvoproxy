//go:build darwin && !cgo

package secretstore

import (
	"context"
	"strings"
	"testing"
)

func TestDarwinMasterKeyFailsClosed(t *testing.T) {
	source := NewPlatformMasterKeySource("ignored")
	if source.Backend() != "macos-keychain-unavailable" {
		t.Fatalf("unexpected backend %q", source.Backend())
	}
	_, err := source.Load(context.Background())
	if err == nil || !strings.Contains(err.Error(), "requires a CGO-enabled") {
		t.Fatalf("expected unavailable error, got %v", err)
	}
}
