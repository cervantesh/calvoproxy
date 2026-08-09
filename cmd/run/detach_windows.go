//go:build windows

package main

import "syscall"

func detachConsole() error {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	freeConsole := kernel32.NewProc("FreeConsole")
	_, _, _ = freeConsole.Call()
	return nil
}
