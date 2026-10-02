//go:build linux || darwin

package download

import "syscall"

func freeSpace(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return spaceBytes(stat.Bavail, uint64(stat.Bsize)), nil
}
