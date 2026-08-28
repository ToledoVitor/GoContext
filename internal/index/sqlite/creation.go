package sqlite

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ToledoVitor/GoContext/internal/index"
)

var (
	errStoreCreationFailed         = errors.New("sqlite store creation failed")
	errStoreCreationCleanup        = errors.New("sqlite store creation cleanup failed")
	errStorePublicationCollision   = errors.New("sqlite store publication collision")
	errStoreReadinessCommitted     = errors.New("sqlite store readiness was committed with maintenance failure")
	errStoreReadinessIndeterminate = errors.New("sqlite store readiness durability is indeterminate")
	errStoreReadinessCleanup       = errors.New("sqlite store readiness local cleanup failed")
)

type privateStagingFile interface {
	Name() string
	Chmod(fs.FileMode) error
	Stat() (fs.FileInfo, error)
	Close() error
}

type storeFileOperations struct {
	remove        func(string) error
	syncDirectory func(string) error
}

func defaultStoreFileOperations() storeFileOperations {
	return storeFileOperations{
		remove:        os.Remove,
		syncDirectory: syncStoreDirectory,
	}
}

func resolvedStoreFileOperations(overrides *storeFileOperations) storeFileOperations {
	operations := defaultStoreFileOperations()
	if overrides == nil {
		return operations
	}
	if overrides.remove != nil {
		operations.remove = overrides.remove
	}
	if overrides.syncDirectory != nil {
		operations.syncDirectory = overrides.syncDirectory
	}
	return operations
}

type stagingDatabase struct {
	path       string
	info       fs.FileInfo
	owned      bool
	cleanupErr error
}

func createPrivateStagingDatabase(
	directory string,
	factory func(string, string) (privateStagingFile, error),
) (stagingDatabase, error) {
	if factory == nil {
		factory = func(directory, pattern string) (privateStagingFile, error) {
			return os.CreateTemp(directory, pattern)
		}
	}
	file, err := factory(directory, ".index-v2.sqlite3.*.staging")
	if err != nil {
		return stagingDatabase{}, err
	}
	staging := stagingDatabase{path: file.Name(), owned: true}
	if filepath.Dir(staging.path) != directory {
		staging.cleanupErr = file.Close()
		return staging, errors.Join(errors.New("staging database escaped private directory"), staging.cleanupErr)
	}
	var operationErrs []error
	if err := file.Chmod(0o600); err != nil {
		operationErrs = append(operationErrs, err)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		operationErrs = append(operationErrs, statErr)
	} else {
		staging.info = info
	}
	staging.cleanupErr = file.Close()
	return staging, errors.Join(errors.Join(operationErrs...), staging.cleanupErr)
}

type storePublicationResult struct {
	targetVisible    bool
	targetCreated    bool
	durable          bool
	temporaryRemoved bool
	cleanupErr       error
	temporaryInfo    fs.FileInfo
	temporaryPath    string
}

type creatorArtifacts struct {
	stagingPath           string
	stagingInfo           fs.FileInfo
	databasePath          string
	databaseInfo          fs.FileInfo
	databaseTargetCreated bool
	sidecarTemporary      string
	sidecarTemporaryInfo  fs.FileInfo
}

func cleanupCreatorArtifacts(
	directory string,
	artifacts creatorArtifacts,
	operations storeFileOperations,
	namespaceLock *storeNamespaceLock,
) error {
	if !namespaceLock.isHeld() {
		return errStoreNamespaceLockCleanup
	}
	var cleanupErrs []error
	remove := func(path string) {
		if path == "" {
			return
		}
		if err := operations.remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			cleanupErrs = append(cleanupErrs, err)
		}
	}

	if artifacts.stagingPath != "" {
		currentInfo, err := os.Lstat(artifacts.stagingPath)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			cleanupErrs = append(cleanupErrs, err)
		case artifacts.stagingInfo == nil || !os.SameFile(artifacts.stagingInfo, currentInfo):
			cleanupErrs = append(cleanupErrs, errors.New("staging database ownership changed"))
		default:
			remove(artifacts.stagingPath + "-wal")
			remove(artifacts.stagingPath + "-shm")
			remove(artifacts.stagingPath + "-journal")
			remove(artifacts.stagingPath)
		}
	}
	if artifacts.databaseTargetCreated {
		currentInfo, err := os.Lstat(artifacts.databasePath)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			remove(artifacts.databasePath + "-wal")
			remove(artifacts.databasePath + "-shm")
			remove(artifacts.databasePath + "-journal")
		case err != nil:
			cleanupErrs = append(cleanupErrs, err)
		case artifacts.databaseInfo == nil || !os.SameFile(artifacts.databaseInfo, currentInfo):
			cleanupErrs = append(cleanupErrs, errors.New("published database ownership changed"))
		default:
			remove(artifacts.databasePath + "-wal")
			remove(artifacts.databasePath + "-shm")
			remove(artifacts.databasePath + "-journal")
			remove(artifacts.databasePath)
		}
	}
	if artifacts.sidecarTemporary != "" {
		currentInfo, err := os.Lstat(artifacts.sidecarTemporary)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			cleanupErrs = append(cleanupErrs, err)
		case artifacts.sidecarTemporaryInfo == nil || !os.SameFile(artifacts.sidecarTemporaryInfo, currentInfo):
			cleanupErrs = append(cleanupErrs, errors.New("identity temporary ownership changed"))
		default:
			remove(artifacts.sidecarTemporary)
		}
	}
	if err := operations.syncDirectory(directory); err != nil {
		cleanupErrs = append(cleanupErrs, err)
	}
	return errors.Join(cleanupErrs...)
}

func storeCreationFailure(cleanupErrs ...error) error {
	categories := []error{index.ErrReindexRequired, errStoreCreationFailed}
	for _, cleanupErr := range cleanupErrs {
		if cleanupErr != nil {
			categories = append(categories, errStoreCreationCleanup)
			break
		}
	}
	return errors.Join(categories...)
}

func storeReadinessFailure(durable bool, cleanupErrs ...error) error {
	var categories []error
	if durable {
		categories = append(categories, errStoreReadinessCommitted)
	} else {
		categories = append(categories, errStoreReadinessIndeterminate)
	}
	for _, cleanupErr := range cleanupErrs {
		if cleanupErr != nil {
			categories = append(categories, errStoreReadinessCleanup)
			break
		}
	}
	return errors.Join(categories...)
}

func removeOwnedTemporaryStoreFile(
	path, directory string,
	expectedInfo fs.FileInfo,
	operations storeFileOperations,
) error {
	currentInfo, statErr := os.Lstat(path)
	var removeErr error
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
	case statErr != nil:
		removeErr = statErr
	case expectedInfo == nil || !os.SameFile(expectedInfo, currentInfo):
		removeErr = errors.New("temporary store file ownership changed")
	default:
		removeErr = operations.remove(path)
		if errors.Is(removeErr, fs.ErrNotExist) {
			removeErr = nil
		}
	}
	syncErr := operations.syncDirectory(directory)
	return errors.Join(removeErr, syncErr)
}

func cleanupSQLiteSidecars(
	databasePath, directory string,
	expectedInfo fs.FileInfo,
	operations storeFileOperations,
	namespaceLock *storeNamespaceLock,
) error {
	if !namespaceLock.isHeld() {
		return errStoreNamespaceLockCleanup
	}
	currentInfo, err := os.Lstat(databasePath)
	if err != nil || expectedInfo == nil || !os.SameFile(expectedInfo, currentInfo) {
		return errors.New("sqlite sidecar ownership is unproven")
	}
	var cleanupErrs []error
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := operations.remove(databasePath + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	if err := operations.syncDirectory(directory); err != nil {
		cleanupErrs = append(cleanupErrs, err)
	}
	return errors.Join(cleanupErrs...)
}
