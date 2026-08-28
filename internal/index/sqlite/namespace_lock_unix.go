//go:build !windows

package sqlite

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockStoreNamespaceFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX)
}

func unlockStoreNamespaceFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
