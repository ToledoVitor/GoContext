//go:build !windows

package sqlite

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockStoreNamespaceFile(ctx context.Context, file *os.File) error {
	if ctx.Done() == nil {
		return unix.Flock(int(file.Fd()), unix.LOCK_EX)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		if err := waitForStoreNamespaceLockRetry(ctx); err != nil {
			return err
		}
	}
}

func unlockStoreNamespaceFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
