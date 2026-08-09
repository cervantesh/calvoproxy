//go:build darwin && !cgo

package secretstore

import (
	"context"
	"errors"
)

type darwinMasterKeySource struct{}

func newPlatformMasterKeySource(string) MasterKeySource { return &darwinMasterKeySource{} }

func (*darwinMasterKeySource) Backend() string { return "macos-keychain-unavailable" }

func (*darwinMasterKeySource) Load(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("macOS Keychain backend requires a CGO-enabled macOS build")
}
