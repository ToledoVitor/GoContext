//go:build windows

package main

import "golang.org/x/sys/windows"

func replaceRollbackMarker(temporary, target, _ string) error {
	from, err := windows.UTF16PtrFromString(temporary)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		from,
		to,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

// syncMarkerDirectory is a documented best-effort no-op on Windows. Go does
// not expose a reliable directory fsync there. Marker publication itself uses
// MoveFileEx with MOVEFILE_WRITE_THROUGH in replaceRollbackMarker.
func syncMarkerDirectory(string) error {
	return nil
}
