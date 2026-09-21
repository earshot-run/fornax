//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// Old conhost does not parse ANSI; ask it for virtual-terminal processing.
// Returns false when the handle is not a console or the call fails.
func vtReady(f *os.File) bool {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	setConsoleMode := kernel32.NewProc("SetConsoleMode")
	var mode uint32
	handle := syscall.Handle(f.Fd())
	r, _, _ := getConsoleMode.Call(uintptr(handle), uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		return false
	}
	const enableVT = 0x0004
	r, _, _ = setConsoleMode.Call(uintptr(handle), uintptr(mode|enableVT))
	return r != 0
}
