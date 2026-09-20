//go:build darwin

package main

import (
	"encoding/binary"
	"syscall"
)

func memoryBytes() int64 {
	value, err := syscall.Sysctl("hw.memsize")
	if err != nil {
		return 0
	}
	raw := []byte(value)
	// Sysctl returns the integer as a byte string with trailing NULs trimmed.
	if len(raw) > 8 {
		return 0
	}
	var buf [8]byte
	copy(buf[:], raw)
	return int64(binary.LittleEndian.Uint64(buf[:]))
}
