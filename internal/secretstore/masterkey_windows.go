//go:build windows

package secretstore

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

const (
	masterKeySize            = 32
	cryptprotectUIForbidden  = 0x1
	windowsMasterKeyFileMode = 0o600
)

var (
	crypt32                  = syscall.NewLazyDLL("crypt32.dll")
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procCryptProtectData     = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData   = crypt32.NewProc("CryptUnprotectData")
	procLocalFree            = kernel32.NewProc("LocalFree")
	windowsMasterKeyFileLock sync.Mutex
)

type dataBlob struct {
	length uint32
	data   *byte
}

type windowsMasterKeySource struct {
	blobPath string
}

func newPlatformMasterKeySource(vaultPath string) MasterKeySource {
	return &windowsMasterKeySource{blobPath: vaultPath + ".masterkey.dpapi"}
}

func (s *windowsMasterKeySource) Backend() string { return "windows-dpapi-current-user" }

func (s *windowsMasterKeySource) Load(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	windowsMasterKeyFileLock.Lock()
	defer windowsMasterKeyFileLock.Unlock()

	protected, err := os.ReadFile(s.blobPath)
	if err == nil {
		return unprotectWindowsData(protected)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read DPAPI master-key blob: %w", err)
	}

	key := make([]byte, masterKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	protected, err = protectWindowsData(key)
	if err != nil {
		clearBytes(key)
		return nil, err
	}
	if err := writeNewPrivateFile(s.blobPath, protected); err != nil {
		if errors.Is(err, os.ErrExist) {
			clearBytes(key)
			winner, readErr := os.ReadFile(s.blobPath)
			if readErr != nil {
				return nil, fmt.Errorf("read concurrently created DPAPI master-key blob: %w", readErr)
			}
			return unprotectWindowsData(winner)
		}
		clearBytes(key)
		return nil, err
	}
	return key, nil
}

func protectWindowsData(plaintext []byte) ([]byte, error) {
	in := bytesBlob(plaintext)
	var out dataBlob
	ok, _, callErr := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0,
		0,
		0,
		0,
		cryptprotectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if ok == 0 {
		return nil, fmt.Errorf("DPAPI CryptProtectData: %w", nonzeroSyscallError(callErr))
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.data)))
	return copyBlob(out), nil
}

func unprotectWindowsData(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 {
		return nil, errors.New("DPAPI master-key blob is empty")
	}
	in := bytesBlob(ciphertext)
	var out dataBlob
	ok, _, callErr := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0,
		0,
		0,
		0,
		cryptprotectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if ok == 0 {
		return nil, fmt.Errorf("DPAPI CryptUnprotectData: %w", nonzeroSyscallError(callErr))
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.data)))
	key := copyBlob(out)
	if len(key) != masterKeySize {
		clearBytes(key)
		return nil, fmt.Errorf("DPAPI master key has %d bytes, want %d", len(key), masterKeySize)
	}
	return key, nil
}

func bytesBlob(value []byte) dataBlob {
	if len(value) == 0 {
		return dataBlob{}
	}
	return dataBlob{length: uint32(len(value)), data: &value[0]}
}

func copyBlob(blob dataBlob) []byte {
	if blob.length == 0 || blob.data == nil {
		return nil
	}
	return append([]byte(nil), unsafe.Slice(blob.data, int(blob.length))...)
}

func nonzeroSyscallError(err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		return errors.New("windows returned an unspecified error")
	}
	return err
}

func writeNewPrivateFile(path string, value []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create master-key directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, windowsMasterKeyFileMode)
	if err != nil {
		return fmt.Errorf("create DPAPI master-key blob: %w", err)
	}
	remove := true
	defer func() {
		_ = f.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.Write(value); err != nil {
		return fmt.Errorf("write DPAPI master-key blob: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync DPAPI master-key blob: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close DPAPI master-key blob: %w", err)
	}
	remove = false
	return nil
}

func clearBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
