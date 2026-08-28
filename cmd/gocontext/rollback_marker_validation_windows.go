//go:build windows

package main

import "io/fs"

// Windows FileMode exposes only the read-only attribute as synthetic POSIX
// bits. Regular-file, reparse-point, identity, and schema checks remain common.
func validateRollbackMarkerPlatformMode(fs.FileInfo) error {
	return nil
}
