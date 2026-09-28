//go:build !darwin && !linux && !windows

package modelrt

func MemoryBytes() int64 {
	return 0
}

func VRAMBytes() int64 {
	return 0
}
