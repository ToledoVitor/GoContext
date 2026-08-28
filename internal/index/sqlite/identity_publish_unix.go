//go:build !windows

package sqlite

import "os"

func publishStoreIdentitySidecar(temporary, target, directory string) error {
	if err := os.Link(temporary, target); err != nil {
		return err
	}
	if err := os.Remove(temporary); err != nil {
		_ = os.Remove(target)
		return err
	}
	if err := syncStoreDirectory(directory); err != nil {
		_ = os.Remove(target)
		_ = syncStoreDirectory(directory)
		return err
	}
	return nil
}

func syncStoreDirectory(directory string) error {
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
