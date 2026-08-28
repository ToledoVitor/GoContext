//go:build !windows

package main

import "os"

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
