//go:build !windows

package core

import (
	"os"

	"golang.org/x/sys/unix"
)

// tryLockFile takes an exclusive advisory lock without blocking. The kernel
// releases it when the process exits, however it exits.
func tryLockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
