//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceRollbackMarkerUnixReplacesAndPersistsDirectoryEntry(t *testing.T) {
	directory := t.TempDir()
	temporary := filepath.Join(directory, "marker.tmp")
	target := filepath.Join(directory, "marker.json")
	if err := os.WriteFile(temporary, []byte("new-marker"), 0o600); err != nil {
		t.Fatalf("WriteFile(temporary) error = %v", err)
	}
	if err := os.WriteFile(target, []byte("old-marker"), 0o600); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}

	if err := replaceRollbackMarker(temporary, target, directory); err != nil {
		t.Fatalf("replaceRollbackMarker() error = %v", err)
	}
	payload, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target) error = %v", err)
	}
	if string(payload) != "new-marker" {
		t.Fatalf("target payload = %q, want new-marker", payload)
	}
	if _, err := os.Lstat(temporary); !os.IsNotExist(err) {
		t.Fatalf("Lstat(temporary) error = %v, want removed by replacement", err)
	}
}
