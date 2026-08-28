//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadRollbackMarkerWindowsAcceptsRegularMarkerAndRejectsNonRegularEntry(t *testing.T) {
	storeDirectory := t.TempDir()
	repositoryID := "windows-rollback-repository"
	digest := sha256.Sum256([]byte("windows-corpus"))
	marker := rollbackMarker{
		Version:          rollbackMarkerSchemaVersion,
		RepositoryHash:   repositoryHash(repositoryID),
		ScanPolicy:       "windows-policy",
		CorpusRevision:   hex.EncodeToString(digest[:]),
		ActiveGeneration: hex.EncodeToString(digest[:]),
	}
	path := rollbackMarkerPath(storeDirectory, repositoryID)
	writeRollbackTestMarker(t, path, marker)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(marker) error = %v", err)
	}
	if err := validateRollbackMarkerPlatformMode(info); err != nil {
		t.Fatalf("validateRollbackMarkerPlatformMode(Windows regular marker) error = %v", err)
	}

	got, err := readRollbackMarker(context.Background(), storeDirectory, repositoryID)
	if err != nil {
		t.Fatalf("readRollbackMarker(Windows regular marker) error = %v", err)
	}
	if got != marker {
		t.Fatalf("readRollbackMarker(Windows regular marker) = %#v, want %#v", got, marker)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove(marker) error = %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir(marker path) error = %v", err)
	}
	if _, err := readRollbackMarker(context.Background(), storeDirectory, repositoryID); !errors.Is(err, errInvalidRollbackMarker) {
		t.Fatalf("readRollbackMarker(Windows directory) error = %v, want invalid marker", err)
	}
}

func TestReadRollbackMarkerWindowsRejectsSymlinkWhenAvailable(t *testing.T) {
	storeDirectory := t.TempDir()
	repositoryID := "windows-symlink-repository"
	path := rollbackMarkerPath(storeDirectory, repositoryID)
	target := filepath.Join(storeDirectory, "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("Symlink unavailable to Windows test user: %v", err)
	}
	if _, err := readRollbackMarker(context.Background(), storeDirectory, repositoryID); !errors.Is(err, errInvalidRollbackMarker) {
		t.Fatalf("readRollbackMarker(Windows symlink) error = %v, want invalid marker", err)
	}
}
