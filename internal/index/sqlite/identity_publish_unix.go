//go:build !windows

package sqlite

import (
	"errors"
	"io/fs"
	"os"
)

func publishStoreIdentitySidecar(temporary, target, directory string) error {
	_, err := publishStoreFileExclusive(temporary, target, directory, defaultStoreFileOperations(), nil)
	return err
}

func publishStoreFileExclusive(
	temporary, target, directory string,
	operations storeFileOperations,
	afterTargetVisible func(string) error,
) (storePublicationResult, error) {
	if err := os.Link(temporary, target); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return storePublicationResult{targetVisible: true}, errStorePublicationCollision
		}
		return storePublicationResult{}, err
	}
	result := storePublicationResult{targetVisible: true, targetCreated: true}
	var operationErrs []error
	if afterTargetVisible != nil {
		if err := afterTargetVisible(target); err != nil {
			operationErrs = append(operationErrs, err)
			result.cleanupErr = errors.Join(result.cleanupErr, err)
		}
	}
	if err := operations.remove(temporary); err != nil {
		result.cleanupErr = errors.Join(result.cleanupErr, err)
		operationErrs = append(operationErrs, err)
	} else {
		result.temporaryRemoved = true
	}
	if err := operations.syncDirectory(directory); err != nil {
		operationErrs = append(operationErrs, err)
	} else {
		result.durable = true
	}
	return result, errors.Join(operationErrs...)
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
