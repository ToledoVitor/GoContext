//go:build windows

package sqlite

import (
	"errors"

	"golang.org/x/sys/windows"
)

const storePublicationMoveFlags = windows.MOVEFILE_WRITE_THROUGH

func publishStoreIdentitySidecar(temporary, target, _ string) error {
	_, err := publishStoreFileExclusive(temporary, target, "", defaultStoreFileOperations(), nil)
	return err
}

func publishStoreFileExclusive(
	temporary, target, _ string,
	_ storeFileOperations,
	afterTargetVisible func(string) error,
) (storePublicationResult, error) {
	from, err := windows.UTF16PtrFromString(temporary)
	if err != nil {
		return storePublicationResult{}, err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return storePublicationResult{}, err
	}
	// Omitting MOVEFILE_REPLACE_EXISTING makes identity publication exclusive;
	// WRITE_THROUGH asks Windows to flush the move before returning.
	if err := windows.MoveFileEx(from, to, storePublicationMoveFlags); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return storePublicationResult{targetVisible: true}, errStorePublicationCollision
		}
		return storePublicationResult{}, err
	}
	result := storePublicationResult{
		targetVisible:    true,
		targetCreated:    true,
		durable:          true,
		temporaryRemoved: true,
	}
	if afterTargetVisible != nil {
		if err := afterTargetVisible(target); err != nil {
			result.cleanupErr = err
			return result, err
		}
	}
	return result, nil
}

// Windows publication uses MOVEFILE_WRITE_THROUGH above. Go does not expose a
// reliable directory flush for cleanup deletions, so deletion sync is best effort.
func syncStoreDirectory(string) error {
	return nil
}
