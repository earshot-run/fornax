//go:build !darwin && !linux && !windows

package main

func memoryBytes() int64 {
	return 0
}
