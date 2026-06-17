//go:build windows

package auth

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// Windows has no flock(2); use LockFileEx on a dedicated ".lock" file for the
// same advisory, interprocess mutual exclusion the Unix path gets from flock.
// writeFileToken relies on this to serialize its read-modify-write (and the
// shared temp-file write that precedes the rename), so a no-op would let
// concurrent logins/refreshes lose an update.

// flockShared acquires a shared (read) lock on path+".lock".
func flockShared(path string) (func(), error) {
	return flockOpen(path+".lock", 0)
}

// flockExclusive acquires an exclusive (write) lock on path+".lock".
func flockExclusive(path string) (func(), error) {
	return flockOpen(path+".lock", windows.LOCKFILE_EXCLUSIVE_LOCK)
}

func flockOpen(lockPath string, flags uint32) (func(), error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	// Lock the entire file range, blocking until the lock is available
	// (no LOCKFILE_FAIL_IMMEDIATELY), matching flock's blocking semantics.
	//
	// os.OpenFile yields a synchronous handle (Go does not pass
	// FILE_FLAG_OVERLAPPED), so LockFileEx blocks until the lock is granted and
	// never returns ERROR_IO_PENDING — that pending/GetOverlappedResult path
	// only applies to handles opened for asynchronous I/O. Treating any error
	// as failure is therefore correct here; this matches the long-standing
	// github.com/gofrs/flock implementation.
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, maxUint32, maxUint32, new(windows.Overlapped)); err != nil {
		f.Close()
		return nil, fmt.Errorf("acquire file lock: %w", err)
	}
	return func() {
		//nolint:errcheck // unlock errors on close are not actionable
		windows.UnlockFileEx(windows.Handle(f.Fd()), 0, maxUint32, maxUint32, new(windows.Overlapped))
		f.Close()
	}, nil
}

const maxUint32 = ^uint32(0)
