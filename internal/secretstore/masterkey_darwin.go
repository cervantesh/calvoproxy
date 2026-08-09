//go:build darwin && cgo

package secretstore

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"unsafe"
)

const darwinKeychainService = "CalvoProxy Provider Vault"
const darwinMasterKeySize = 32

type darwinMasterKeySource struct{ account string }

func newPlatformMasterKeySource(vaultPath string) MasterKeySource {
	sum := sha256.Sum256([]byte(vaultPath))
	return &darwinMasterKeySource{account: hex.EncodeToString(sum[:])}
}

func (*darwinMasterKeySource) Backend() string { return "macos-keychain" }

func (s *darwinMasterKeySource) Load(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key, err := s.find(); err == nil {
		return key, nil
	} else if !errors.Is(err, errDarwinItemNotFound) {
		return nil, err
	}

	key := make([]byte, darwinMasterKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate macOS vault master key: %w", err)
	}
	if err := ctx.Err(); err != nil {
		clear(key)
		return nil, err
	}
	status := addDarwinKeychainItem(s.account, key)
	if status == C.errSecDuplicateItem {
		clear(key)
		return s.find()
	}
	if status != C.errSecSuccess {
		clear(key)
		return nil, fmt.Errorf("store macOS Keychain master key: OSStatus %d", int32(status))
	}
	return key, nil
}

var errDarwinItemNotFound = errors.New("macOS Keychain item not found")

func (s *darwinMasterKeySource) find() ([]byte, error) {
	service := C.CString(darwinKeychainService)
	account := C.CString(s.account)
	defer C.free(unsafe.Pointer(service))
	defer C.free(unsafe.Pointer(account))
	var length C.UInt32
	var data unsafe.Pointer
	status := C.SecKeychainFindGenericPassword(nil,
		C.UInt32(len(darwinKeychainService)), service,
		C.UInt32(len(s.account)), account, &length, &data, nil)
	if status == C.errSecItemNotFound {
		return nil, errDarwinItemNotFound
	}
	if status != C.errSecSuccess {
		return nil, fmt.Errorf("read macOS Keychain master key: OSStatus %d", int32(status))
	}
	defer C.SecKeychainItemFreeContent(nil, data)
	if int(length) != darwinMasterKeySize {
		return nil, fmt.Errorf("read macOS Keychain master key: %w", ErrInvalidKey)
	}
	return C.GoBytes(data, C.int(length)), nil
}

func addDarwinKeychainItem(accountValue string, key []byte) C.OSStatus {
	service := C.CString(darwinKeychainService)
	account := C.CString(accountValue)
	defer C.free(unsafe.Pointer(service))
	defer C.free(unsafe.Pointer(account))
	return C.SecKeychainAddGenericPassword(nil,
		C.UInt32(len(darwinKeychainService)), service,
		C.UInt32(len(accountValue)), account,
		C.UInt32(len(key)), unsafe.Pointer(&key[0]), nil)
}
