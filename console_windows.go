//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleOutputCP = kernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleCP       = kernel32.NewProc("SetConsoleCP")
	procSetConsoleTitleW   = kernel32.NewProc("SetConsoleTitleW")
	procGetConsoleMode     = kernel32.NewProc("GetConsoleMode")
)

func setupConsole() {
	// UTF-8, чтобы русский текст корректно отображался и читался в cmd.
	_, _, _ = procSetConsoleOutputCP.Call(65001)
	_, _, _ = procSetConsoleCP.Call(65001)

	if title, err := syscall.UTF16PtrFromString("YouTube Downloader"); err == nil {
		_, _, _ = procSetConsoleTitleW.Call(uintptr(unsafe.Pointer(title)))
	}
}

func isTerminalInput() bool {
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(uintptr(syscall.Stdin), uintptr(unsafe.Pointer(&mode)))
	return r != 0
}
