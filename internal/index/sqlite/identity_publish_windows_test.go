//go:build windows

package sqlite

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestStorePublicationMoveIsExclusiveAndWriteThrough(t *testing.T) {
	if storePublicationMoveFlags&windows.MOVEFILE_WRITE_THROUGH == 0 {
		t.Fatal("store publication is not write-through")
	}
	if storePublicationMoveFlags&windows.MOVEFILE_REPLACE_EXISTING != 0 {
		t.Fatal("store publication can replace an existing target")
	}
}

func TestStorePublicationMovesFileWithoutReplacingReadyTarget(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, databaseName)
	firstTemporary := filepath.Join(directory, "first.staging")
	firstPayload := []byte("first store")
	if err := os.WriteFile(firstTemporary, firstPayload, 0o600); err != nil {
		t.Fatalf("WriteFile(first staging) error = %v", err)
	}
	result, err := publishStoreFileExclusive(
		firstTemporary,
		target,
		directory,
		defaultStoreFileOperations(),
		nil,
	)
	if err != nil || !result.targetVisible || !result.targetCreated || !result.durable || !result.temporaryRemoved {
		t.Fatalf("publishStoreFileExclusive(first) = (%+v, %v), want published", result, err)
	}
	if _, err := os.Lstat(firstTemporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Lstat(first staging) error = %v, want absent", err)
	}

	secondTemporary := filepath.Join(directory, "second.staging")
	if err := os.WriteFile(secondTemporary, []byte("second store"), 0o600); err != nil {
		t.Fatalf("WriteFile(second staging) error = %v", err)
	}
	result, err = publishStoreFileExclusive(
		secondTemporary,
		target,
		directory,
		defaultStoreFileOperations(),
		nil,
	)
	if !errors.Is(err, errStorePublicationCollision) || !result.targetVisible || result.targetCreated || result.durable || result.temporaryRemoved {
		t.Fatalf("publishStoreFileExclusive(second) = (%+v, %v), want visible uncreated collision", result, err)
	}
	if _, err := os.Lstat(secondTemporary); err != nil {
		t.Fatalf("Lstat(second staging) error = %v, want retained for caller cleanup", err)
	}
	payload, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target) error = %v", err)
	}
	if !bytes.Equal(payload, firstPayload) {
		t.Fatalf("target payload = %q, want original %q", payload, firstPayload)
	}
}
