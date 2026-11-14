//go:build windows

package host

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive LockFileEx on the first byte of the state lock file; unlockFile must name the
// same byte.
func lockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &ol)
}

func unlockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}
