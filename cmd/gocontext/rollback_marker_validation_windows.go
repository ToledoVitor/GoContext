//go:build windows

package main

import "io/fs"

// Windows rollback markers remain fail-closed until GoContext can create and
// validate an owner-only DACL on a Windows runtime. Inherited ACLs are not
// privacy proof.
func validateRollbackMarkerPlatform() error {
	return errRollbackMarkerPlatformUnsupported
}

func validateRollbackMarkerPlatformMode(fs.FileInfo) error {
	return errRollbackMarkerPlatformUnsupported
}
