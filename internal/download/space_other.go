//go:build !linux && !darwin && !windows

package download

import "errors"

func freeSpace(string) (int64, error) {
	return 0, errors.New("disk capacity unavailable on this platform")
}
