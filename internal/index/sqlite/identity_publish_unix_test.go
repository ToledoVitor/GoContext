//go:build !windows

package sqlite

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestStorePublicationSeparatesTargetVisibilityFromDurabilityFailure(t *testing.T) {
	directory := t.TempDir()
	temporary := filepath.Join(directory, "database.staging")
	target := filepath.Join(directory, databaseName)
	if err := os.WriteFile(temporary, []byte("initialized store"), 0o600); err != nil {
		t.Fatalf("WriteFile(staging) error = %v", err)
	}
	privateSyncCanary := errors.New("PRIVATE_PUBLICATION_DURABILITY_CANARY")
	result, err := publishStoreFileExclusive(
		temporary,
		target,
		directory,
		storeFileOperations{
			remove:        os.Remove,
			syncDirectory: func(string) error { return privateSyncCanary },
		},
		nil,
	)
	if err == nil || !result.targetVisible || !result.targetCreated || result.durable || !result.temporaryRemoved {
		t.Fatalf("publishStoreFileExclusive() = (%+v, %v), want visible created but indeterminate target", result, err)
	}
	if _, statErr := os.Lstat(target); statErr != nil {
		t.Fatalf("Lstat(visible target) error = %v", statErr)
	}
	if _, statErr := os.Lstat(temporary); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Lstat(temporary) error = %v, want removed name", statErr)
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

func TestFailedDatabaseCleanupSerializesAReplacementCreator(t *testing.T) {
	directory := t.TempDir()
	privateSyncCanary := errors.New("PRIVATE_DATABASE_SYNC_CANARY")
	databaseRemoved := make(chan struct{})
	allowCleanupToFinish := make(chan struct{})
	firstResult := make(chan error, 1)
	go func() {
		syncCalls := 0
		store, err := newStore(directory, storeOpenHooks{
			fileOperations: &storeFileOperations{
				remove: func(path string) error {
					if filepath.Base(path) != databaseName {
						return os.Remove(path)
					}
					if err := os.Remove(path); err != nil {
						return err
					}
					close(databaseRemoved)
					<-allowCleanupToFinish
					return nil
				},
				syncDirectory: func(canonicalDirectory string) error {
					syncCalls++
					if syncCalls == 2 {
						return privateSyncCanary
					}
					return syncStoreDirectory(canonicalDirectory)
				},
			},
		})
		if store != nil {
			err = errors.Join(err, store.Close())
		}
		firstResult <- err
	}()
	<-databaseRemoved

	type storeResult struct {
		store *Store
		err   error
	}
	secondResult := make(chan storeResult, 1)
	go func() {
		store, err := NewStore(directory)
		secondResult <- storeResult{store: store, err: err}
	}()
	select {
	case result := <-secondResult:
		if result.store != nil {
			_ = result.store.Close()
		}
		close(allowCleanupToFinish)
		<-firstResult
		t.Fatalf("replacement creator completed inside serialized cleanup: %v", result.err)
	case <-time.After(200 * time.Millisecond):
	}
	close(allowCleanupToFinish)
	if err := <-firstResult; !errors.Is(err, errStoreCreationFailed) {
		t.Fatalf("first newStore(database sync failure) error = %v, want creation failure", err)
	}
	result := <-secondResult
	if result.err != nil || result.store == nil {
		t.Fatalf("replacement NewStore() = (%v, %v), want success after cleanup release", result.store, result.err)
	}
	if err := result.store.Close(); err != nil {
		t.Fatalf("Close(replacement store) error = %v", err)
	}
	reopened, err := OpenExisting(directory)
	if err != nil {
		t.Fatalf("OpenExisting(replacement store) error = %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close(reopened replacement) error = %v", err)
	}
}

func TestFreshSidecarPublicationSyncFailurePreservesVisiblePairAsIndeterminate(t *testing.T) {
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
	if !errors.Is(err, errStoreReadinessIndeterminate) {
		t.Fatalf("newStore(sidecar publication sync failure) error = %v, want readiness indeterminate", err)
	}
	if errorTreeContains(err, privateSyncCanary.Error()) {
		t.Fatalf("newStore(sidecar publication sync failure) exposes %q", privateSyncCanary)
	}
	entries, readErr := internalDirectoryEntries(directory)
	wantEntries := []string{storeIdentitySidecar, databaseName}
	if readErr != nil || !reflect.DeepEqual(entries, wantEntries) {
		t.Fatalf("sidecar publication sync failure entries = %v, %v; want preserved %v", entries, readErr, wantEntries)
	}
}

func TestFreshSidecarPostLinkFailurePreservesPairAlreadyAdoptedByOpener(t *testing.T) {
	directory := t.TempDir()
	privateSyncCanary := errors.New("PRIVATE_POST_LINK_SYNC_CANARY")
	var adopted *Store
	var adoptErr error
	syncCalls := 0
	store, err := newStore(directory, storeOpenHooks{
		fileOperations: &storeFileOperations{
			remove: os.Remove,
			syncDirectory: func(canonicalDirectory string) error {
				syncCalls++
				if syncCalls == 3 {
					adopted, adoptErr = OpenExisting(canonicalDirectory)
					return privateSyncCanary
				}
				return syncStoreDirectory(canonicalDirectory)
			},
		},
	})
	if store != nil {
		_ = store.Close()
		t.Fatal("newStore(post-link failure) returned store, want nil indeterminate result")
	}
	if adoptErr != nil || adopted == nil {
		t.Fatalf("OpenExisting(after sidecar link) = (%v, %v), want adopted store", adopted, adoptErr)
	}
	if err := adopted.Close(); err != nil {
		t.Fatalf("Close(adopted store) error = %v", err)
	}
	if !errors.Is(err, errStoreReadinessIndeterminate) {
		t.Fatalf("newStore(post-link failure) error = %v, want readiness indeterminate", err)
	}
	if errors.Is(err, errStoreCreationCleanup) || errors.Is(err, errStoreReadinessCleanup) {
		t.Fatalf("newStore(post-link failure) error = %v, must not claim cleanup failure", err)
	}
	if errorTreeContains(err, privateSyncCanary.Error()) {
		t.Fatalf("newStore(post-link failure) exposes %q", privateSyncCanary)
	}
	for _, path := range []string{
		filepath.Join(directory, databaseName),
		filepath.Join(directory, storeIdentitySidecar),
	} {
		if _, statErr := os.Lstat(path); statErr != nil {
			t.Fatalf("Lstat(preserved %s) error = %v", filepath.Base(path), statErr)
		}
	}
	reopened, reopenErr := OpenExisting(directory)
	if reopenErr != nil {
		t.Fatalf("OpenExisting(preserved pair) error = %v", reopenErr)
	}
	if closeErr := reopened.Close(); closeErr != nil {
		t.Fatalf("Close(reopened pair) error = %v", closeErr)
	}
}

func TestFreshSidecarTemporaryRemovalFailurePreservesDurablePairAndReportsMaintenance(t *testing.T) {
	directory := t.TempDir()
	privateRemoveCanary := errors.New("PRIVATE_SIDECAR_TEMP_REMOVE_CANARY")
	sidecarPath := filepath.Join(directory, storeIdentitySidecar)
	temporaryRemoveCalls := 0
	store, err := newStore(directory, storeOpenHooks{
		fileOperations: &storeFileOperations{
			remove: func(path string) error {
				base := filepath.Base(path)
				if strings.HasPrefix(base, ".index-v2.identity.") && strings.HasSuffix(base, ".tmp") {
					temporaryRemoveCalls++
					if temporaryRemoveCalls == 1 {
						return privateRemoveCanary
					}
				}
				return os.Remove(path)
			},
			syncDirectory: syncStoreDirectory,
		},
	})
	if store != nil {
		_ = store.Close()
		t.Fatal("newStore(sidecar temporary removal failure) returned store, want nil maintenance result")
	}
	if !errors.Is(err, errStoreReadinessCommitted) || !errors.Is(err, errStoreReadinessCleanup) {
		t.Fatalf("newStore(sidecar temporary removal failure) error = %v, want committed readiness and local-cleanup categories", err)
	}
	if errorTreeContains(err, privateRemoveCanary.Error()) {
		t.Fatalf("newStore(sidecar temporary removal failure) exposes %q", privateRemoveCanary)
	}
	if temporaryRemoveCalls != 2 {
		t.Fatalf("sidecar temporary remove calls = %d, want retry after target visibility", temporaryRemoveCalls)
	}
	for _, path := range []string{filepath.Join(directory, databaseName), sidecarPath} {
		if _, statErr := os.Lstat(path); statErr != nil {
			t.Fatalf("Lstat(preserved %s) error = %v", filepath.Base(path), statErr)
		}
	}
	reopened, reopenErr := OpenExisting(directory)
	if reopenErr != nil {
		t.Fatalf("OpenExisting(committed pair) error = %v", reopenErr)
	}
	if closeErr := reopened.Close(); closeErr != nil {
		t.Fatalf("Close(committed pair) error = %v", closeErr)
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
