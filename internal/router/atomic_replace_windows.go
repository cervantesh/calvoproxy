//go:build windows

package router

import "golang.org/x/sys/windows"

// atomicReplaceFile replaces dst without first unlinking the last valid copy.
// MoveFileEx with REPLACE_EXISTING is the Windows equivalent of POSIX rename.
func atomicReplaceFile(src, dst string) error {
	return windows.Rename(src, dst)
}
