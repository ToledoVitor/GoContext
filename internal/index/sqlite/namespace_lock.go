package sqlite

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const storeNamespaceLockName = ".index-v2.lock"

var (
	errStoreNamespaceLockFailed  = errors.New("sqlite store namespace lock failed")
	errStoreNamespaceLockCleanup = errors.New("sqlite store namespace lock cleanup failed")

	processStoreLocks = struct {
		sync.Mutex
		entries map[string]*processStoreLock
	}{entries: make(map[string]*processStoreLock)}
)

// The stable private lock file serializes every cooperating GoContext open and
// create operation that can observe or change the store namespace. The store
// directory is owner-only; a process running with the same owner credentials
// that bypasses this lock is outside the pathname-integrity trust boundary.
// The lock file is never removed because unlinking a live lock inode could split
// later processes across distinct locks.
type storeNamespaceLock struct {
	path         string
	file         *os.File
	processEntry *processStoreLock
	held         bool
}

type processStoreLock struct {
	token chan struct{}
	refs  int
}

type storeNamespaceLockHooks struct {
	beforeProcessWait func(string)
	afterFileOpen     func(string) error
	beforeFileLock    func(string) error
}

func acquireStoreNamespaceLock(directory string, allowCreate bool) (*storeNamespaceLock, error) {
	return acquireStoreNamespaceLockContext(context.Background(), directory, allowCreate)
}

func acquireStoreNamespaceLockContext(ctx context.Context, directory string, allowCreate bool) (*storeNamespaceLock, error) {
	return acquireStoreNamespaceLockContextWithHooks(ctx, directory, allowCreate, storeNamespaceLockHooks{})
}

func acquireStoreNamespaceLockContextWithHooks(
	ctx context.Context,
	directory string,
	allowCreate bool,
	hooks storeNamespaceLockHooks,
) (*storeNamespaceLock, error) {
	path := filepath.Join(directory, storeNamespaceLockName)
	processEntry, err := acquireProcessStoreLock(ctx, path, hooks.beforeProcessWait)
	if err != nil {
		return nil, err
	}
	fail := func(file *os.File, operationErr error) (*storeNamespaceLock, error) {
		if file != nil {
			_ = file.Close()
		}
		releaseProcessStoreLock(path, processEntry)
		if errors.Is(operationErr, context.Canceled) || errors.Is(operationErr, context.DeadlineExceeded) {
			return nil, operationErr
		}
		return nil, errStoreNamespaceLockFailed
	}

	file, created, err := openStoreNamespaceLock(path, allowCreate)
	if err != nil {
		return fail(nil, err)
	}
	fileInfo, err := file.Stat()
	if err != nil || validateStoreNamespaceLockInfo(fileInfo) != nil {
		return fail(file, err)
	}
	if hooks.afterFileOpen != nil {
		if err := hooks.afterFileOpen(path); err != nil {
			return fail(file, err)
		}
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || validateStoreNamespaceLockInfo(pathInfo) != nil || !os.SameFile(fileInfo, pathInfo) {
		return fail(file, err)
	}
	if hooks.beforeFileLock != nil {
		if err := hooks.beforeFileLock(path); err != nil {
			return fail(file, err)
		}
	}
	if err := lockStoreNamespaceFile(ctx, file); err != nil {
		return fail(file, err)
	}
	lockedPathInfo, err := os.Lstat(path)
	if err != nil || validateStoreNamespaceLockInfo(lockedPathInfo) != nil || !os.SameFile(fileInfo, lockedPathInfo) {
		_ = unlockStoreNamespaceFile(file)
		return fail(file, err)
	}
	// A fresh creator may be adopting a lock left by an earlier failed attempt.
	// Flush it again before the ready pair can depend on its survival.
	if created || allowCreate {
		if err := file.Sync(); err != nil {
			_ = unlockStoreNamespaceFile(file)
			return fail(file, err)
		}
		if err := syncStoreDirectory(directory); err != nil {
			_ = unlockStoreNamespaceFile(file)
			return fail(file, err)
		}
	}
	return &storeNamespaceLock{
		path:         path,
		file:         file,
		processEntry: processEntry,
		held:         true,
	}, nil
}

func openStoreNamespaceLock(path string, allowCreate bool) (*os.File, bool, error) {
	for {
		info, err := os.Lstat(path)
		switch {
		case err == nil:
			if validateStoreNamespaceLockInfo(info) != nil {
				return nil, false, errStoreNamespaceLockFailed
			}
			file, openErr := os.OpenFile(path, os.O_RDWR, 0)
			return file, false, openErr
		case !errors.Is(err, fs.ErrNotExist):
			return nil, false, err
		case !allowCreate:
			return nil, false, fs.ErrNotExist
		}

		file, createErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(createErr, fs.ErrExist) {
			continue
		}
		if createErr != nil {
			return nil, false, createErr
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return nil, true, err
		}
		return file, true, nil
	}
}

func validateStoreNamespaceLockInfo(info fs.FileInfo) error {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != 0 {
		return errStoreNamespaceLockFailed
	}
	if err := validatePrivateMode(info); err != nil {
		return errStoreNamespaceLockFailed
	}
	return nil
}

func (lock *storeNamespaceLock) release() error {
	if lock == nil || !lock.held {
		return nil
	}
	lock.held = false
	unlockErr := unlockStoreNamespaceFile(lock.file)
	closeErr := lock.file.Close()
	releaseProcessStoreLock(lock.path, lock.processEntry)
	lock.file = nil
	lock.processEntry = nil
	if unlockErr != nil || closeErr != nil {
		return errStoreNamespaceLockCleanup
	}
	return nil
}

func (lock *storeNamespaceLock) isHeld() bool {
	return lock != nil && lock.held
}

func acquireProcessStoreLock(ctx context.Context, path string, beforeWait func(string)) (*processStoreLock, error) {
	processStoreLocks.Lock()
	entry := processStoreLocks.entries[path]
	if entry == nil {
		entry = &processStoreLock{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		processStoreLocks.entries[path] = entry
	}
	entry.refs++
	processStoreLocks.Unlock()
	if beforeWait != nil {
		beforeWait(path)
	}
	select {
	case <-ctx.Done():
		releaseProcessStoreLockReference(path, entry)
		return nil, ctx.Err()
	case <-entry.token:
		if err := ctx.Err(); err != nil {
			entry.token <- struct{}{}
			releaseProcessStoreLockReference(path, entry)
			return nil, err
		}
		return entry, nil
	}
}

func releaseProcessStoreLock(path string, entry *processStoreLock) {
	entry.token <- struct{}{}
	releaseProcessStoreLockReference(path, entry)
}

func releaseProcessStoreLockReference(path string, entry *processStoreLock) {
	processStoreLocks.Lock()
	entry.refs--
	if entry.refs == 0 {
		delete(processStoreLocks.entries, path)
	}
	processStoreLocks.Unlock()
}

// Context-aware platform waiters retry the atomic OS lock operation against
// this already-open, validated inode. They never poll for lock-path or database
// publication, and acquisition is followed by the same pathname identity check.
func waitForStoreNamespaceLockRetry(ctx context.Context) error {
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
