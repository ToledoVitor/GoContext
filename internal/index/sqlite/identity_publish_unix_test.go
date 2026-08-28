//go:build !windows

package sqlite

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/index"
)

func TestPublishStoreIdentitySidecarIsExclusive(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, storeIdentitySidecar)
	firstTemporary := filepath.Join(directory, "first.tmp")
	firstPayload := []byte("first identity")
	if err := os.WriteFile(firstTemporary, firstPayload, 0o600); err != nil {
		t.Fatalf("WriteFile(first temporary) error = %v", err)
	}
	if err := publishStoreIdentitySidecar(firstTemporary, target, directory); err != nil {
		t.Fatalf("publishStoreIdentitySidecar(first) error = %v", err)
	}
	if _, err := os.Lstat(firstTemporary); !os.IsNotExist(err) {
		t.Fatalf("Lstat(first temporary) error = %v, want removed", err)
	}

	secondTemporary := filepath.Join(directory, "second.tmp")
	if err := os.WriteFile(secondTemporary, []byte("second identity"), 0o600); err != nil {
		t.Fatalf("WriteFile(second temporary) error = %v", err)
	}
	if err := publishStoreIdentitySidecar(secondTemporary, target, directory); err == nil {
		t.Fatal("publishStoreIdentitySidecar(second) error = nil, want exclusive collision")
	}
	payload, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target) error = %v", err)
	}
	if !bytes.Equal(payload, firstPayload) {
		t.Fatalf("target payload = %q, want original %q", payload, firstPayload)
	}
}

func TestFreshDatabasePublicationSyncFailureNeverReportsReady(t *testing.T) {
	directory := t.TempDir()
	privateSyncCanary := errors.New("PRIVATE_DATABASE_PUBLICATION_SYNC_CANARY")
	syncCalls := 0
	store, err := newStore(directory, storeOpenHooks{
		fileOperations: &storeFileOperations{
			remove: os.Remove,
			syncDirectory: func(string) error {
				syncCalls++
				if syncCalls == 2 {
					return privateSyncCanary
				}
				return nil
			},
		},
	})
	if store != nil {
		_ = store.Close()
		t.Fatal("newStore(database publication sync failure) returned store, want nil")
	}
	if !errors.Is(err, index.ErrReindexRequired) || !errors.Is(err, errStoreCreationFailed) {
		t.Fatalf("newStore(database publication sync failure) error = %v, want creation failure", err)
	}
	if errorTreeContains(err, privateSyncCanary.Error()) {
		t.Fatalf("newStore(database publication sync failure) exposes %q", privateSyncCanary)
	}
	entries, readErr := internalDirectoryEntries(directory)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("database publication sync failure entries = %v, %v; want empty", entries, readErr)
	}
}

func TestFreshDatabasePublicationRemovalFailureIsFalseReadyAndJoined(t *testing.T) {
	directory := t.TempDir()
	privateSyncCanary := errors.New("PRIVATE_DATABASE_SYNC_CANARY")
	privateRemoveCanary := errors.New("PRIVATE_DATABASE_REMOVE_CANARY")
	databasePath := filepath.Join(directory, databaseName)
	syncCalls := 0
	store, err := newStore(directory, storeOpenHooks{
		fileOperations: &storeFileOperations{
			remove: func(path string) error {
				if filepath.Base(path) == databaseName {
					return privateRemoveCanary
				}
				return os.Remove(path)
			},
			syncDirectory: func(string) error {
				syncCalls++
				if syncCalls == 2 {
					return privateSyncCanary
				}
				return nil
			},
		},
	})
	if store != nil {
		_ = store.Close()
		t.Fatal("newStore(database removal failure) returned store, want nil")
	}
	if !errors.Is(err, errStoreCreationFailed) || !errors.Is(err, errStoreCreationCleanup) {
		t.Fatalf("newStore(database removal failure) error = %v, want creation and cleanup categories", err)
	}
	for _, canary := range []string{privateSyncCanary.Error(), privateRemoveCanary.Error()} {
		if errorTreeContains(err, canary) {
			t.Fatalf("newStore(database removal failure) exposes %q", canary)
		}
	}
	opened, openErr := OpenExisting(directory)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("OpenExisting(false-ready database) returned store, want nil")
	}
	if !errors.Is(openErr, index.ErrReindexRequired) {
		t.Fatalf("OpenExisting(false-ready database) error = %v, want ErrReindexRequired", openErr)
	}
	if err := os.Remove(databasePath); err != nil {
		t.Fatalf("Remove(false-ready database fixture) error = %v", err)
	}
}

func TestFreshSidecarPublicationSyncFailureNeverReportsReady(t *testing.T) {
	directory := t.TempDir()
	privateSyncCanary := errors.New("PRIVATE_SIDECAR_PUBLICATION_SYNC_CANARY")
	syncCalls := 0
	store, err := newStore(directory, storeOpenHooks{
		fileOperations: &storeFileOperations{
			remove: os.Remove,
			syncDirectory: func(string) error {
				syncCalls++
				if syncCalls == 3 {
					return privateSyncCanary
				}
				return nil
			},
		},
	})
	if store != nil {
		_ = store.Close()
		t.Fatal("newStore(sidecar publication sync failure) returned store, want nil")
	}
	if !errors.Is(err, errStoreCreationFailed) {
		t.Fatalf("newStore(sidecar publication sync failure) error = %v, want creation failure", err)
	}
	if errorTreeContains(err, privateSyncCanary.Error()) {
		t.Fatalf("newStore(sidecar publication sync failure) exposes %q", privateSyncCanary)
	}
	entries, readErr := internalDirectoryEntries(directory)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("sidecar publication sync failure entries = %v, %v; want empty", entries, readErr)
	}
}

func TestFreshSidecarPublicationRemovalFailureIsFalseReadyAndJoined(t *testing.T) {
	directory := t.TempDir()
	privateSyncCanary := errors.New("PRIVATE_SIDECAR_SYNC_CANARY")
	privateRemoveCanary := errors.New("PRIVATE_SIDECAR_REMOVE_CANARY")
	sidecarPath := filepath.Join(directory, storeIdentitySidecar)
	syncCalls := 0
	store, err := newStore(directory, storeOpenHooks{
		fileOperations: &storeFileOperations{
			remove: func(path string) error {
				if filepath.Base(path) == storeIdentitySidecar {
					return privateRemoveCanary
				}
				return os.Remove(path)
			},
			syncDirectory: func(string) error {
				syncCalls++
				if syncCalls == 3 {
					return privateSyncCanary
				}
				return nil
			},
		},
	})
	if store != nil {
		_ = store.Close()
		t.Fatal("newStore(sidecar removal failure) returned store, want nil")
	}
	if !errors.Is(err, errStoreCreationFailed) || !errors.Is(err, errStoreCreationCleanup) {
		t.Fatalf("newStore(sidecar removal failure) error = %v, want creation and cleanup categories", err)
	}
	for _, canary := range []string{privateSyncCanary.Error(), privateRemoveCanary.Error()} {
		if errorTreeContains(err, canary) {
			t.Fatalf("newStore(sidecar removal failure) exposes %q", canary)
		}
	}
	if _, statErr := os.Lstat(filepath.Join(directory, databaseName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Lstat(false-ready database) error = %v, want absent", statErr)
	}
	if _, statErr := os.Lstat(sidecarPath); statErr != nil {
		t.Fatalf("Lstat(false-ready sidecar) error = %v, want retained failed cleanup fixture", statErr)
	}
	if err := os.Remove(sidecarPath); err != nil {
		t.Fatalf("Remove(false-ready sidecar fixture) error = %v", err)
	}
}

func TestFreshCreationHasNoFallibleFilesystemStepAfterSidecarDurability(t *testing.T) {
	directory := t.TempDir()
	sidecarDurable := false
	afterDurabilityCalls := 0
	privateAfterCanary := errors.New("PRIVATE_POST_SIDECAR_CANARY")
	store, err := newStore(directory, storeOpenHooks{
		fileOperations: &storeFileOperations{
			remove: func(path string) error {
				if sidecarDurable {
					afterDurabilityCalls++
					return privateAfterCanary
				}
				return os.Remove(path)
			},
			syncDirectory: func(canonicalDirectory string) error {
				if sidecarDurable {
					afterDurabilityCalls++
					return privateAfterCanary
				}
				if err := syncStoreDirectory(canonicalDirectory); err != nil {
					return err
				}
				if _, err := os.Lstat(filepath.Join(canonicalDirectory, storeIdentitySidecar)); err == nil {
					sidecarDurable = true
				}
				return nil
			},
		},
	})
	if err != nil {
		t.Fatalf("newStore(post-sidecar guard) error = %v", err)
	}
	if !sidecarDurable {
		t.Fatal("sidecar durability boundary was not observed")
	}
	if afterDurabilityCalls != 0 {
		t.Fatalf("filesystem calls after sidecar durability = %d, want 0", afterDurabilityCalls)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(post-sidecar guard) error = %v", err)
	}
}
