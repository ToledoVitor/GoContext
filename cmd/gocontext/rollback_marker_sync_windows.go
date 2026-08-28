//go:build windows

package main

// syncMarkerDirectory is a documented best-effort no-op on Windows. Go does
// not expose a reliable directory fsync there; the marker file itself is
// flushed before the atomic rename.
func syncMarkerDirectory(string) error {
	return nil
}
