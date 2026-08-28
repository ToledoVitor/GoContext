//go:build !windows

package main

import "os"

func replaceRollbackMarker(temporary, target, directory string) error {
	if err := os.Rename(temporary, target); err != nil {
		return err
	}
	if err := syncMarkerDirectory(directory); err != nil {
		_ = os.Remove(target)
		_ = syncMarkerDirectory(directory)
		return err
	}
	return nil
}

func syncMarkerDirectory(directory string) error {
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
