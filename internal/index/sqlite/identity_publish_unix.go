//go:build !windows

package sqlite

import (
	"errors"
	"io/fs"
	"os"
)

func publishStoreIdentitySidecar(temporary, target, directory string) error {
	_, err := publishStoreFileExclusive(temporary, target, directory, defaultStoreFileOperations())
	return err
}

func publishStoreFileExclusive(
	temporary, target, directory string,
	operations storeFileOperations,
) (storePublicationResult, error) {
	if err := os.Link(temporary, target); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return storePublicationResult{}, errStorePublicationCollision
		}
		return storePublicationResult{}, err
	}
	result := storePublicationResult{published: true}
	if err := operations.remove(temporary); err != nil {
		result.cleanupErr = err
		return result, err
	}
	if err := operations.syncDirectory(directory); err != nil {
		removeErr := operations.remove(target)
		if errors.Is(removeErr, fs.ErrNotExist) {
			removeErr = nil
		}
		retrySyncErr := operations.syncDirectory(directory)
		result.cleanupErr = errors.Join(removeErr, retrySyncErr)
		return result, err
	}
	return result, nil
}

func syncStoreDirectory(directory string) error {
	root, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := root.Sync()
	closeErr := root.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
