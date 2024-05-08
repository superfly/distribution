//go:build !linux
// +build !linux

package s3

import (
	"time"
)

func setTCPUserTimeout(fd uintptr, timeout time.Duration) error {
	return nil
}
