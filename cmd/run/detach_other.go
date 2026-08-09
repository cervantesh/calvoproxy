//go:build !windows

package main

func detachConsole() error { return nil }
