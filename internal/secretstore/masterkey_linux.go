//go:build linux

package secretstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const (
	masterKeySize              = 32
	systemdMasterKeyCredential = "calvoproxy-vault-master-key"
	masterKeyFileEnvironment   = "PROXY_VAULT_MASTER_KEY_FILE"
)

type linuxMasterKeySource struct{}

func newPlatformMasterKeySource(string) MasterKeySource { return &linuxMasterKeySource{} }

func (s *linuxMasterKeySource) Backend() string {
	if os.Getenv("CREDENTIALS_DIRECTORY") != "" {
		return "linux-systemd-credential"
	}
	return "linux-mounted-file"
}

func (s *linuxMasterKeySource) Load(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if directory := os.Getenv("CREDENTIALS_DIRECTORY"); directory != "" {
		return readPrivateMasterKey(filepath.Join(directory, systemdMasterKeyCredential))
	}
	if path := os.Getenv(masterKeyFileEnvironment); path != "" {
		return readPrivateMasterKey(path)
	}
	return nil, errors.New("master key unavailable: configure a systemd credential or PROXY_VAULT_MASTER_KEY_FILE; automatic generation is disabled on Linux")
}

func readPrivateMasterKey(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, errors.New("master-key file must not be a symbolic link")
		}
		return nil, fmt.Errorf("open master-key file: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("open master-key file: invalid file descriptor")
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect master-key file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("master-key path must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("master-key file permissions %04o allow group or world access", info.Mode().Perm())
	}
	key, err := io.ReadAll(io.LimitReader(f, masterKeySize+1))
	if err != nil {
		return nil, fmt.Errorf("read master-key file: %w", err)
	}
	if len(key) != masterKeySize {
		clearBytes(key)
		return nil, fmt.Errorf("master-key file has %d bytes, want exactly %d raw bytes", len(key), masterKeySize)
	}
	return key, nil
}

func clearBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
