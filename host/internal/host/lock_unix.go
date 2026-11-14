//go:build !windows

package host

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive flock on the state lock file.
func lockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }

func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
