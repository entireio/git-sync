//go:build !windows

package auth

import (
	"fmt"
	"os"
	"syscall"
)

// flockShared acquires a shared (read) lock on path+".lock".
func flockShared(path string) (func(), error) {
	return flockOpen(path+".lock", syscall.LOCK_SH)
}

// flockExclusive acquires an exclusive (write) lock on path+".lock".
func flockExclusive(path string) (func(), error) {
	return flockOpen(path+".lock", syscall.LOCK_EX)
}

func flockOpen(lockPath string, how int) (func(), error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, fmt.Errorf("acquire file lock: %w", err)
	}
	return func() {
		//nolint:errcheck // unlock errors on close are not actionable
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
