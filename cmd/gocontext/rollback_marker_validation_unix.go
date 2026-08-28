//go:build !windows

package main

import "io/fs"

func validateRollbackMarkerPlatformMode(info fs.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return errInvalidRollbackMarker
	}
	return nil
}
