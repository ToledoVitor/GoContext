//go:build windows

package sqlite

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func lockStoreNamespaceFile(ctx context.Context, file *os.File) error {
	if ctx.Done() == nil {
		var overlapped windows.Overlapped
		return windows.LockFileEx(
			windows.Handle(file.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK,
			0,
			1,
			0,
			&overlapped,
		)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var overlapped windows.Overlapped
		err := windows.LockFileEx(
			windows.Handle(file.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0,
			1,
			0,
			&overlapped,
		)
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return err
		}
		if err := waitForStoreNamespaceLockRetry(ctx); err != nil {
			return err
		}
	}
}

func unlockStoreNamespaceFile(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}
