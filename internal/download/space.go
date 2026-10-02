package download

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// FreeSpace reports bytes available to this user on the destination filesystem.
// A destination that does not exist yet uses its nearest existing parent.
func FreeSpace(path string) (int64, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	for {
		info, err := os.Stat(path)
		if err == nil {
			if !info.IsDir() {
				path = filepath.Dir(path)
			}
			return freeSpace(path)
		}
		if !os.IsNotExist(err) || filepath.Dir(path) == path {
			return 0, err
		}
		path = filepath.Dir(path)
	}
}

func spaceBytes(blocks, size uint64) int64 {
	if size != 0 && blocks > math.MaxInt64/size {
		return math.MaxInt64
	}
	return int64(blocks * size)
}

// Probe failures do not block a download: filesystems may not expose capacity.
// This is a preflight estimate, not a reservation against concurrent writers.
func checkSpace(path string, needed int64, probe func(string) (int64, error)) error {
	if needed <= 0 {
		return nil
	}
	available, err := probe(filepath.Dir(path))
	if err != nil {
		return nil
	}
	if needed > available {
		return fmt.Errorf("not enough disk space for %s: need %s more, %s available; free space and retry to reuse partial files", filepath.Base(path), formatSpace(needed), formatSpace(available))
	}
	return nil
}

func formatSpace(bytes int64) string {
	if bytes >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(bytes)/(1<<30))
	}
	if bytes >= 1<<20 {
		return fmt.Sprintf("%.1f MiB", float64(bytes)/(1<<20))
	}
	return fmt.Sprintf("%d bytes", bytes)
}
