//go:build linux || windows

package modelrt

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	vramOnce  sync.Once
	vramBytes int64
)

// The total memory of the first NVIDIA GPU, or 0 when nvidia-smi is absent
// or the driver does not answer. Only the fit check and `doctor` call it —
// engine selection never runs anything. Probed once per process, bounded so
// a wedged driver cannot stall a command.
func VRAMBytes() int64 {
	vramOnce.Do(func() { vramBytes = probeVRAM() })
	return vramBytes
}

func probeVRAM() int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	mib, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64)
	if err != nil || mib <= 0 {
		return 0
	}
	return mib << 20
}
