//go:build windows

package core

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFile takes an exclusive lock without blocking. Windows releases it
// when the process exits, however it exits.
func tryLockFile(f *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
}
