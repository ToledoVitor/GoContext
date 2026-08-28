//go:build windows

package main

import "testing"

func TestSyncMarkerDirectoryWindowsDoesNotRejectDirectory(t *testing.T) {
	if err := syncMarkerDirectory(t.TempDir()); err != nil {
		t.Fatalf("syncMarkerDirectory() error = %v, want best-effort success", err)
	}
}
