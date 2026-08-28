//go:build windows

package sqlite

import (
	"errors"

	"golang.org/x/sys/windows"
)

const storePublicationMoveFlags = windows.MOVEFILE_WRITE_THROUGH

func publishStoreIdentitySidecar(temporary, target, _ string) error {
	_, err := publishStoreFileExclusive(temporary, target, "", defaultStoreFileOperations())
	return err
}

func publishStoreFileExclusive(
	temporary, target, _ string,
	_ storeFileOperations,
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
			return storePublicationResult{}, errStorePublicationCollision
		}
		return storePublicationResult{}, err
	}
	return storePublicationResult{published: true}, nil
}

// Windows publication uses MOVEFILE_WRITE_THROUGH above. Go does not expose a
// reliable directory flush for cleanup deletions, so deletion sync is best effort.
func syncStoreDirectory(string) error {
	return nil
}
