//go:build windows

package sqlite

import "golang.org/x/sys/windows"

const identitySidecarMoveFlags = windows.MOVEFILE_WRITE_THROUGH

func publishStoreIdentitySidecar(temporary, target, _ string) error {
	from, err := windows.UTF16PtrFromString(temporary)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	// Omitting MOVEFILE_REPLACE_EXISTING makes identity publication exclusive;
	// WRITE_THROUGH asks Windows to flush the move before returning.
	return windows.MoveFileEx(from, to, identitySidecarMoveFlags)
}

// Windows publication uses MOVEFILE_WRITE_THROUGH above. Go does not expose a
// reliable directory flush for cleanup deletions, so deletion sync is best effort.
func syncStoreDirectory(string) error {
	return nil
}
