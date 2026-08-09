//go:build !windows

package router

import "os"

func atomicReplaceFile(src, dst string) error {
	return os.Rename(src, dst)
}
