//go:build windows

package modelrt

import (
	"syscall"
	"unsafe"
)

type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

func MemoryBytes() int64 {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GlobalMemoryStatusEx")
	var status memoryStatusEx
	status.length = uint32(unsafe.Sizeof(status))
	ret, _, _ := proc.Call(uintptr(unsafe.Pointer(&status)))
	if ret == 0 {
		return 0
	}
	return int64(status.totalPhys)
}
