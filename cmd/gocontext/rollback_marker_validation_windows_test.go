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

func TestReadRollbackMarkerWindowsFailsClosedWithoutOwnerOnlyDACLValidation(t *testing.T) {
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
	if err := validateRollbackMarkerPlatformMode(info); !errors.Is(err, errRollbackMarkerPlatformUnsupported) {
		t.Fatalf("validateRollbackMarkerPlatformMode(Windows regular marker) error = %v, want unsupported", err)
	}

	if _, err := readRollbackMarker(context.Background(), storeDirectory, repositoryID); !errors.Is(err, errRollbackMarkerPlatformUnsupported) {
		t.Fatalf("readRollbackMarker(Windows regular marker) error = %v, want unsupported", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove(marker) error = %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("Mkdir(marker path) error = %v", err)
	}
	directoryInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(marker directory) error = %v", err)
	}
	if err := validatePrivateRollbackMarker(directoryInfo); !errors.Is(err, errInvalidRollbackMarker) {
		t.Fatalf("validatePrivateRollbackMarker(Windows directory) error = %v, want invalid marker", err)
	}
	if _, err := readRollbackMarker(context.Background(), storeDirectory, repositoryID); !errors.Is(err, errRollbackMarkerPlatformUnsupported) {
		t.Fatalf("readRollbackMarker(Windows directory) error = %v, want unsupported", err)
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
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(marker symlink) error = %v", err)
	}
	if err := validatePrivateRollbackMarker(info); !errors.Is(err, errInvalidRollbackMarker) {
		t.Fatalf("validatePrivateRollbackMarker(Windows symlink) error = %v, want invalid marker", err)
	}
	if _, err := readRollbackMarker(context.Background(), storeDirectory, repositoryID); !errors.Is(err, errRollbackMarkerPlatformUnsupported) {
		t.Fatalf("readRollbackMarker(Windows symlink) error = %v, want unsupported", err)
	}
}
