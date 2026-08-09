//go:build windows

package secretstore

import "golang.org/x/sys/windows"

func atomicReplaceFile(src, dst string) error {
	return windows.Rename(src, dst)
}

func syncParentDir(string) error { return nil }
