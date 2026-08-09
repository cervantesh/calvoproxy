//go:build !windows

package secretstore

import "os"

func atomicReplaceFile(src, dst string) error {
	return os.Rename(src, dst)
}

func syncParentDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
