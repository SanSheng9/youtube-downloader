//go:build !windows

package main

import "os"

func setupConsole() {}

func isTerminalInput() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
