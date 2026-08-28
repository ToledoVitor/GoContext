package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/index"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func bootstrapDataSourceName(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func initializeNewDatabase(path string) error {
	database, err := sql.Open("sqlite", bootstrapDataSourceName(path))
	if err != nil {
		return err
	}
	if _, err := database.ExecContext(context.Background(), schemaSQL); err != nil {
		_ = database.Close()
		return err
	}
	return database.Close()
}

func createPrivateDatabaseFile(path string) (bool, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, file.Close()
}

func TestPreExistingIdentityLessStoreRequiresReindexWithoutMutation(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, databaseName)
	created, err := createPrivateDatabaseFile(databasePath)
	if err != nil || !created {
		t.Fatalf("createPrivateDatabaseFile() = %v, %v; want created", created, err)
	}
	if err := initializeNewDatabase(databasePath); err != nil {
		t.Fatalf("initializeNewDatabase() error = %v", err)
	}
	if err := os.Chmod(databasePath, 0o600); err != nil {
		t.Fatalf("Chmod(database) error = %v", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod(directory) error = %v", err)
	}
	beforeBytes, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("ReadFile(database before) error = %v", err)
	}
	beforeEntries, err := internalDirectoryEntries(directory)
	if err != nil {
		t.Fatalf("ReadDir(before) error = %v", err)
	}

	for _, open := range []struct {
		name string
		open func(string) (*Store, error)
	}{
		{name: "read-only", open: OpenExisting},
		{name: "writer", open: NewStore},
	} {
		t.Run(open.name, func(t *testing.T) {
			store, err := open.open(directory)
			if store != nil {
				_ = store.Close()
				t.Fatalf("%s open returned store, want nil", open.name)
			}
			if !errors.Is(err, index.ErrReindexRequired) {
				t.Fatalf("%s open error = %v, want ErrReindexRequired", open.name, err)
			}
		})
	}
	afterBytes, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("ReadFile(database after) error = %v", err)
	}
	if !bytes.Equal(afterBytes, beforeBytes) {
		t.Fatal("identity-less store was mutated")
	}
	afterEntries, err := internalDirectoryEntries(directory)
	if err != nil {
		t.Fatalf("ReadDir(after) error = %v", err)
	}
	if !reflect.DeepEqual(afterEntries, beforeEntries) {
		t.Fatalf("identity-less store entries = %v, want unchanged %v", afterEntries, beforeEntries)
	}
	for _, sidecar := range []string{databasePath + "-wal", databasePath + "-shm", filepath.Join(directory, storeIdentitySidecar)} {
		if _, statErr := os.Lstat(sidecar); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("Lstat(%s) error = %v, want absent", filepath.Base(sidecar), statErr)
		}
	}
}

func TestFreshWriterCreatesPrivateSingletonStoreIdentity(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, databaseName)
	writer, err := NewStore(directory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close(writer) error = %v", err)
	}
	secondWriter, err := NewStore(directory)
	if err != nil {
		t.Fatalf("NewStore(initialized store) error = %v", err)
	}
	if err := secondWriter.Close(); err != nil {
		t.Fatalf("Close(second writer) error = %v", err)
	}
	identityPath := filepath.Join(directory, storeIdentitySidecar)
	payload, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatalf("ReadFile(identity) error = %v", err)
	}
	var sidecar struct {
		Version int    `json:"version"`
		StoreID string `json:"store_id"`
	}
	if err := json.Unmarshal(payload, &sidecar); err != nil {
		t.Fatalf("Unmarshal(identity) error = %v", err)
	}
	if sidecar.Version != 1 || len(sidecar.StoreID) != 64 {
		t.Fatalf("identity sidecar = %#v, want version 1 opaque 32-byte hex ID", sidecar)
	}
	if filepath.IsAbs(storeIdentityRepositoryID(sidecar.StoreID)) {
		t.Fatalf("reserved identity row %q can collide with a canonical absolute repository ID", storeIdentityRepositoryID(sidecar.StoreID))
	}
	identityInfo, err := os.Lstat(identityPath)
	if err != nil {
		t.Fatalf("Lstat(identity) error = %v", err)
	}
	if !identityInfo.Mode().IsRegular() || (runtime.GOOS != "windows" && identityInfo.Mode().Perm() != 0o600) {
		t.Fatalf("identity sidecar mode = %v, want private regular file", identityInfo.Mode())
	}
	db, err := sql.Open("sqlite", inspectionDataSourceName(databasePath))
	if err != nil {
		t.Fatalf("sql.Open(identity inspection) error = %v", err)
	}
	defer db.Close()
	var reservedRows int
	if err := db.QueryRow(`SELECT count(*) FROM repositories WHERE repository_id LIKE 'gocontext:store-identity:v1:%' AND active_generation IS NULL`).Scan(&reservedRows); err != nil {
		t.Fatalf("query reserved identity row error = %v", err)
	}
	if reservedRows != 1 {
		t.Fatalf("reserved identity rows = %d, want 1", reservedRows)
	}

	reader, err := OpenExisting(directory)
	if err != nil {
		t.Fatalf("OpenExisting(initialized store) error = %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close(reader) error = %v", err)
	}
}

func TestWriterDataSourceNeverCreatesOrConfiguresBeforeIdentityVerification(t *testing.T) {
	dsn, err := url.Parse(writerDataSourceName(filepath.Join(t.TempDir(), databaseName)))
	if err != nil {
		t.Fatalf("url.Parse(writer data source) error = %v", err)
	}
	query := dsn.Query()
	if query.Get("mode") != "rw" {
		t.Fatalf("writer mode = %q, want rw", query.Get("mode"))
	}
	if pragmas := query["_pragma"]; len(pragmas) != 0 {
		t.Fatalf("writer data source pragmas = %v, want none before identity verification", pragmas)
	}
}

func TestOpenExistingRejectsUntrustedStoreIdentitySidecars(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string, []byte)
		skip   bool
	}{
		{
			name: "missing",
			mutate: func(t *testing.T, path string, _ []byte) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatalf("Remove(identity) error = %v", err)
				}
			},
		},
		{
			name: "mismatch",
			mutate: func(t *testing.T, path string, _ []byte) {
				t.Helper()
				payload := []byte(`{"version":1,"store_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
				if err := os.WriteFile(path, payload, 0o600); err != nil {
					t.Fatalf("WriteFile(identity mismatch) error = %v", err)
				}
			},
		},
		{
			name: "symlink",
			skip: runtime.GOOS == "windows",
			mutate: func(t *testing.T, path string, original []byte) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "identity-target")
				if err := os.WriteFile(target, original, 0o600); err != nil {
					t.Fatalf("WriteFile(identity target) error = %v", err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatalf("Remove(identity) error = %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatalf("Symlink(identity) error = %v", err)
				}
			},
		},
		{
			name: "nonregular",
			mutate: func(t *testing.T, path string, _ []byte) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatalf("Remove(identity) error = %v", err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("Mkdir(identity path) error = %v", err)
				}
			},
		},
		{
			name: "permissive",
			skip: runtime.GOOS == "windows",
			mutate: func(t *testing.T, path string, _ []byte) {
				t.Helper()
				if err := os.Chmod(path, 0o640); err != nil {
					t.Fatalf("Chmod(identity) error = %v", err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.skip {
				t.Skip("filesystem behavior is not reliable on this platform")
			}
			directory := t.TempDir()
			store, err := NewStore(directory)
			if err != nil {
				t.Fatalf("NewStore() error = %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			identityPath := filepath.Join(directory, storeIdentitySidecar)
			original, err := os.ReadFile(identityPath)
			if err != nil {
				t.Fatalf("ReadFile(identity) error = %v", err)
			}
			test.mutate(t, identityPath, original)

			opened, err := OpenExisting(directory)
			if opened != nil {
				_ = opened.Close()
				t.Fatal("OpenExisting(untrusted identity) returned store, want nil")
			}
			if !errors.Is(err, index.ErrReindexRequired) || strings.Contains(err.Error(), string(original)) {
				t.Fatalf("OpenExisting(untrusted identity) error = %v, want sanitized ErrReindexRequired", err)
			}
		})
	}
}

func TestOpenExistingFailurePreservesOnlySanitizedOperationAndCleanupCategories(t *testing.T) {
	operationCanary := errors.New("PRIVATE_OPEN_OPERATION_CANARY")
	connectionCloseCanary := errors.New("PRIVATE_CONNECTION_CLOSE_CANARY")
	databaseCloseCanary := errors.New("PRIVATE_DATABASE_CLOSE_CANARY")
	err := openExistingFailure(
		errors.Join(index.ErrReindexRequired, operationCanary),
		connectionCloseCanary,
		databaseCloseCanary,
	)
	if !errors.Is(err, index.ErrReindexRequired) || !errors.Is(err, errOpenExistingCleanup) {
		t.Fatalf("openExistingFailure() error = %v, want reindex and cleanup categories", err)
	}
	for _, canary := range []string{operationCanary.Error(), connectionCloseCanary.Error(), databaseCloseCanary.Error()} {
		if errorTreeContains(err, canary) {
			t.Fatalf("openExistingFailure() error tree exposes %q", canary)
		}
	}
}

func TestStoreRejectsReservedIdentityRepositoryIDWithoutCorruptingIdentity(t *testing.T) {
	directory := t.TempDir()
	store, err := NewStore(directory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	reservedID := storeIdentityRepositoryID(strings.Repeat("a", 64))
	generation := internalTestGeneration(t, reservedID, "generation", "reserved.py", "VALUE = 1")
	if err := store.Replace(context.Background(), generation); !errors.Is(err, index.ErrInvalidGeneration) {
		t.Fatalf("Replace(reserved repository) error = %v, want ErrInvalidGeneration", err)
	}
	if _, err := store.ActiveGeneration(context.Background(), reservedID); !errors.Is(err, index.ErrInvalidGeneration) {
		t.Fatalf("ActiveGeneration(reserved repository) error = %v, want ErrInvalidGeneration", err)
	}
	if _, err := store.BindActive(context.Background(), reservedID); !errors.Is(err, index.ErrInvalidGeneration) {
		t.Fatalf("BindActive(reserved repository) error = %v, want ErrInvalidGeneration", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reopened, err := OpenExisting(directory)
	if err != nil {
		t.Fatalf("OpenExisting(after rejection) error = %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close(reopened) error = %v", err)
	}
}

func TestNewStoreValidatesExclusiveCreateCollisionBeforeMutableOpen(t *testing.T) {
	for _, test := range []struct {
		name   string
		schema string
	}{
		{name: "identity-less v1", schema: schemaSQL},
		{name: "future schema", schema: `CREATE TABLE schema_version(version INTEGER NOT NULL); INSERT INTO schema_version VALUES (2)`},
		{name: "unrelated sqlite", schema: `CREATE TABLE unrelated(value TEXT)`},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			var beforeBytes []byte
			var beforeEntries []string
			store, err := newStore(directory, storeOpenHooks{
				beforeCreateDatabase: func(databasePath string) error {
					db, err := sql.Open("sqlite", bootstrapDataSourceName(databasePath))
					if err != nil {
						return err
					}
					if _, err := db.ExecContext(context.Background(), test.schema); err != nil {
						_ = db.Close()
						return err
					}
					if err := db.Close(); err != nil {
						return err
					}
					if err := os.Chmod(databasePath, 0o600); err != nil {
						return err
					}
					beforeBytes, err = os.ReadFile(databasePath)
					if err != nil {
						return err
					}
					beforeEntries, err = internalDirectoryEntries(directory)
					beforeEntries = removeInternalStagingEntries(beforeEntries)
					return err
				},
			})
			if store != nil {
				_ = store.Close()
				t.Fatal("newStore(collision) returned store, want nil")
			}
			if !errors.Is(err, index.ErrReindexRequired) {
				t.Fatalf("newStore(collision) error = %v, want ErrReindexRequired", err)
			}
			databasePath := filepath.Join(directory, databaseName)
			afterBytes, readErr := os.ReadFile(databasePath)
			if readErr != nil {
				t.Fatalf("ReadFile(after collision) error = %v", readErr)
			}
			if !bytes.Equal(afterBytes, beforeBytes) {
				t.Fatal("newStore(collision) mutated colliding database bytes")
			}
			afterEntries, readErr := internalDirectoryEntries(directory)
			if readErr != nil {
				t.Fatalf("ReadDir(after collision) error = %v", readErr)
			}
			if !reflect.DeepEqual(afterEntries, beforeEntries) {
				t.Fatalf("newStore(collision) entries = %v, want unchanged %v", afterEntries, beforeEntries)
			}
			for _, sidecar := range []string{databasePath + "-wal", databasePath + "-shm"} {
				if _, statErr := os.Lstat(sidecar); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("Lstat(%s) error = %v, want absent", filepath.Base(sidecar), statErr)
				}
			}
		})
	}
}

func TestNewStoreDiscardsStagingAndAdoptsOnlyFullyReadyCollision(t *testing.T) {
	preparedDirectory, preparedDatabase := internalIdentityStore(t, "prepared-repository", "prepared-generation")
	preparedSidecar := filepath.Join(preparedDirectory, storeIdentitySidecar)
	directory := t.TempDir()
	store, err := newStore(directory, storeOpenHooks{
		beforeCreateDatabase: func(databasePath string) error {
			if err := os.Rename(preparedDatabase, databasePath); err != nil {
				return err
			}
			return os.Rename(preparedSidecar, filepath.Join(filepath.Dir(databasePath), storeIdentitySidecar))
		},
	})
	if err != nil {
		t.Fatalf("newStore(fully ready collision) error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(adopted collision) error = %v", err)
	}
	entries, err := internalDirectoryEntries(directory)
	if err != nil {
		t.Fatalf("ReadDir(adopted collision) error = %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry, ".index-v2.sqlite3.") {
			t.Fatalf("adopted collision retained staging artifact %q", entry)
		}
	}
	reopened, err := OpenExisting(directory)
	if err != nil {
		t.Fatalf("OpenExisting(adopted collision) error = %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close(reopened collision) error = %v", err)
	}
}

func removeInternalStagingEntries(entries []string) []string {
	filtered := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry, ".index-v2.sqlite3.") && strings.HasSuffix(entry, ".staging") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func TestFreshStoreIdentityPublicationFailureRemovesCreatedArtifacts(t *testing.T) {
	directory := t.TempDir()
	privateCanary := errors.New("PRIVATE_IDENTITY_PUBLICATION_CANARY")
	store, err := newStore(directory, storeOpenHooks{
		beforeIdentitySidecarPublish: func(string) error {
			return privateCanary
		},
	})
	if store != nil {
		_ = store.Close()
		t.Fatal("newStore(identity publication failure) returned store, want nil")
	}
	if !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("newStore(identity publication failure) error = %v, want ErrReindexRequired", err)
	}
	if errorTreeContains(err, privateCanary.Error()) {
		t.Fatalf("newStore(identity publication failure) error exposes %q", privateCanary)
	}
	entries, readErr := internalDirectoryEntries(directory)
	if readErr != nil {
		t.Fatalf("ReadDir(after failed creation) error = %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed fresh store entries = %v, want no created artifacts", entries)
	}
}

func TestFreshStoreIdentityPublicationFailureRetriesTemporaryCleanup(t *testing.T) {
	directory := t.TempDir()
	privateOperationCanary := errors.New("PRIVATE_IDENTITY_PUBLICATION_CANARY")
	privateCleanupCanary := errors.New("PRIVATE_IDENTITY_TEMP_CLEANUP_CANARY")
	temporaryRemoveCalls := 0
	store, err := newStore(directory, storeOpenHooks{
		beforeIdentitySidecarPublish: func(string) error {
			return privateOperationCanary
		},
		fileOperations: &storeFileOperations{
			remove: func(path string) error {
				base := filepath.Base(path)
				if strings.HasPrefix(base, ".index-v2.identity.") && strings.HasSuffix(base, ".tmp") {
					temporaryRemoveCalls++
					if temporaryRemoveCalls == 1 {
						return privateCleanupCanary
					}
				}
				return os.Remove(path)
			},
			syncDirectory: syncStoreDirectory,
		},
	})
	if store != nil {
		_ = store.Close()
		t.Fatal("newStore(identity temp cleanup retry) returned store, want nil")
	}
	if !errors.Is(err, index.ErrReindexRequired) || !errors.Is(err, errStoreCreationFailed) || !errors.Is(err, errStoreCreationCleanup) {
		t.Fatalf("newStore(identity temp cleanup retry) error = %v, want reindex, creation, and cleanup categories", err)
	}
	for _, canary := range []string{privateOperationCanary.Error(), privateCleanupCanary.Error()} {
		if errorTreeContains(err, canary) {
			t.Fatalf("newStore(identity temp cleanup retry) error exposes %q", canary)
		}
	}
	if temporaryRemoveCalls != 2 {
		t.Fatalf("identity temporary remove calls = %d, want 2", temporaryRemoveCalls)
	}
	entries, readErr := internalDirectoryEntries(directory)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("identity temp cleanup retry entries = %v, %v; want empty", entries, readErr)
	}
}

func TestMatchingIdentitySidecarPublicationCollisionNeverReportsReady(t *testing.T) {
	directory := t.TempDir()
	store, err := newStore(directory, storeOpenHooks{
		beforeIdentitySidecarPublish: func(sidecarPath string) error {
			database, err := sql.Open("sqlite", inspectionDataSourceName(filepath.Join(directory, databaseName)))
			if err != nil {
				return err
			}
			identities, identityErr := loadDatabaseIdentities(context.Background(), database)
			closeErr := database.Close()
			if identityErr != nil || closeErr != nil || len(identities) != 1 {
				return errors.New("read published database identity")
			}
			payload, err := json.Marshal(storeIdentityDocument{Version: storeIdentityVersion, StoreID: identities[0]})
			if err != nil {
				return err
			}
			return os.WriteFile(sidecarPath, payload, 0o600)
		},
	})
	if store != nil {
		_ = store.Close()
		t.Fatal("newStore(matching sidecar collision) returned store, want nil")
	}
	if !errors.Is(err, index.ErrReindexRequired) || !errors.Is(err, errStoreCreationFailed) {
		t.Fatalf("newStore(matching sidecar collision) error = %v, want creation failure", err)
	}
	entries, readErr := internalDirectoryEntries(directory)
	if readErr != nil || !reflect.DeepEqual(entries, []string{storeIdentitySidecar}) {
		t.Fatalf("matching sidecar collision entries = %v, %v; want only pre-existing sidecar", entries, readErr)
	}
	if _, statErr := os.Lstat(filepath.Join(directory, databaseName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Lstat(database after sidecar collision) error = %v, want absent", statErr)
	}
	if err := os.Remove(filepath.Join(directory, storeIdentitySidecar)); err != nil {
		t.Fatalf("Remove(pre-existing sidecar fixture) error = %v", err)
	}
}

func TestFreshStorePublishesSidecarOnlyAfterFinalWriterIsReady(t *testing.T) {
	directory := t.TempDir()
	hookCalled := false
	var readinessErr error
	store, err := newStore(directory, storeOpenHooks{
		beforeIdentitySidecarPublish: func(sidecarPath string) error {
			check := func(err error) error {
				readinessErr = err
				return err
			}
			hookCalled = true
			if _, err := os.Lstat(sidecarPath); !errors.Is(err, os.ErrNotExist) {
				return check(errors.New("identity sidecar was visible before readiness publication"))
			}
			databasePath := filepath.Join(directory, databaseName)
			info, err := secureDatabaseInfo(databasePath)
			if err != nil || !info.Mode().IsRegular() {
				return check(errors.New("final database was not privately published before sidecar"))
			}
			database, err := sql.Open("sqlite", writerDataSourceName(databasePath))
			if err != nil {
				return check(err)
			}
			defer database.Close()
			if err := validateSchema(context.Background(), database); err != nil {
				return check(err)
			}
			identities, err := loadDatabaseIdentities(context.Background(), database)
			if err != nil || len(identities) != 1 {
				return check(errors.New("final database identity was not ready"))
			}
			var journalMode string
			if err := database.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
				return check(err)
			}
			if !strings.EqualFold(journalMode, "wal") {
				return check(errors.New("final writer prerequisites were not ready"))
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("newStore() error = %v; readiness check = %v", err, readinessErr)
	}
	if !hookCalled {
		t.Fatal("sidecar readiness hook was not called")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, storeIdentitySidecar)); err != nil {
		t.Fatalf("Lstat(final identity sidecar) error = %v", err)
	}
}

func TestFreshStoreInitializationFailureRollsBackAndRemovesCreatedArtifacts(t *testing.T) {
	directory := t.TempDir()
	privateCanary := errors.New("PRIVATE_IDENTITY_INITIALIZATION_CANARY")
	stagingPath := ""
	store, err := newStore(directory, storeOpenHooks{
		beforeFreshIdentityInsert: func(path string) error {
			stagingPath = path
			if filepath.Base(path) == databaseName {
				t.Fatal("fresh identity initialized at public database target, want private staging path")
			}
			if _, err := os.Lstat(filepath.Join(directory, databaseName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("public database target exists during staging initialization: %v", err)
			}
			return privateCanary
		},
	})
	if store != nil {
		_ = store.Close()
		t.Fatal("newStore(identity initialization failure) returned store, want nil")
	}
	if !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("newStore(identity initialization failure) error = %v, want ErrReindexRequired", err)
	}
	if errorTreeContains(err, privateCanary.Error()) {
		t.Fatalf("newStore(identity initialization failure) error exposes %q", privateCanary)
	}
	entries, readErr := internalDirectoryEntries(directory)
	if readErr != nil {
		t.Fatalf("ReadDir(after failed initialization) error = %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed fresh initialization entries = %v, want no created artifacts", entries)
	}
	if stagingPath == "" {
		t.Fatal("fresh initialization hook did not observe a staging path")
	}
}

type failingPrivateStagingFile struct {
	*os.File
	statErr  error
	closeErr error
}

func (f *failingPrivateStagingFile) Stat() (fs.FileInfo, error) {
	if f.statErr != nil {
		return nil, f.statErr
	}
	return f.File.Stat()
}

func (f *failingPrivateStagingFile) Close() error {
	closeErr := f.File.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return closeErr
}

func TestFreshStagingStatAndCloseFailuresRetainCleanupOwnership(t *testing.T) {
	for _, test := range []struct {
		name      string
		statFails bool
	}{
		{name: "stat", statFails: true},
		{name: "close"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			privateCanary := errors.New("PRIVATE_STAGING_" + strings.ToUpper(test.name) + "_CANARY")
			createdPath := ""
			store, err := newStore(directory, storeOpenHooks{
				createStagingFile: func(directory, pattern string) (privateStagingFile, error) {
					file, err := os.CreateTemp(directory, pattern)
					if err != nil {
						return nil, err
					}
					createdPath = file.Name()
					wrapped := &failingPrivateStagingFile{File: file}
					if test.statFails {
						wrapped.statErr = privateCanary
					} else {
						wrapped.closeErr = privateCanary
					}
					return wrapped, nil
				},
			})
			if store != nil {
				_ = store.Close()
				t.Fatal("newStore(staging failure) returned store, want nil")
			}
			if !errors.Is(err, index.ErrReindexRequired) || !errors.Is(err, errStoreCreationFailed) {
				t.Fatalf("newStore(staging failure) error = %v, want reindex creation failure", err)
			}
			if gotCleanup := errors.Is(err, errStoreCreationCleanup); gotCleanup != !test.statFails {
				t.Fatalf("newStore(staging failure) cleanup category = %v, want %v", gotCleanup, !test.statFails)
			}
			if errorTreeContains(err, privateCanary.Error()) {
				t.Fatalf("newStore(staging failure) error exposes %q", privateCanary)
			}
			if createdPath == "" {
				t.Fatal("staging factory did not create a path")
			}
			entries, readErr := internalDirectoryEntries(directory)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("staging failure entries = %v, %v; want empty", entries, readErr)
			}
		})
	}
}

func TestFreshStagingUsesCanonicalPrivateDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory symlink behavior is not portable on Windows")
	}
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real-store")
	if err := os.Mkdir(realDirectory, 0o755); err != nil {
		t.Fatalf("Mkdir(real store) error = %v", err)
	}
	linkedDirectory := filepath.Join(root, "linked-store")
	if err := os.Symlink(realDirectory, linkedDirectory); err != nil {
		t.Fatalf("Symlink(store) error = %v", err)
	}
	observedDirectory := ""
	store, err := newStore(linkedDirectory, storeOpenHooks{
		createStagingFile: func(directory, pattern string) (privateStagingFile, error) {
			observedDirectory = directory
			return os.CreateTemp(directory, pattern)
		},
	})
	if err != nil {
		t.Fatalf("newStore(canonical staging) error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(canonical staging) error = %v", err)
	}
	canonical, err := filepath.EvalSymlinks(linkedDirectory)
	if err != nil {
		t.Fatalf("EvalSymlinks(linked store) error = %v", err)
	}
	if observedDirectory != canonical {
		t.Fatalf("staging directory = %q, want canonical %q", observedDirectory, canonical)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		t.Fatalf("Stat(canonical store) error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("canonical store mode = %#o, want 0700", got)
	}
}

func TestFreshCreationCleanupFailureIsSanitizedAndJoined(t *testing.T) {
	directory := t.TempDir()
	privateOperationCanary := errors.New("PRIVATE_CREATION_OPERATION_CANARY")
	privateCleanupCanary := errors.New("PRIVATE_CREATION_CLEANUP_CANARY")
	store, err := newStore(directory, storeOpenHooks{
		beforeFreshIdentityInsert: func(string) error {
			return privateOperationCanary
		},
		fileOperations: &storeFileOperations{
			remove: func(string) error { return privateCleanupCanary },
			syncDirectory: func(string) error {
				return nil
			},
		},
	})
	if store != nil {
		_ = store.Close()
		t.Fatal("newStore(cleanup failure) returned store, want nil")
	}
	if !errors.Is(err, index.ErrReindexRequired) || !errors.Is(err, errStoreCreationFailed) || !errors.Is(err, errStoreCreationCleanup) {
		t.Fatalf("newStore(cleanup failure) error = %v, want reindex, creation, and cleanup categories", err)
	}
	for _, canary := range []string{privateOperationCanary.Error(), privateCleanupCanary.Error()} {
		if errorTreeContains(err, canary) {
			t.Fatalf("newStore(cleanup failure) error exposes %q", canary)
		}
	}
	entries, readErr := os.ReadDir(directory)
	if readErr != nil {
		t.Fatalf("ReadDir(cleanup failure) error = %v", readErr)
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
			t.Fatalf("Remove(test cleanup %s) error = %v", entry.Name(), err)
		}
	}
}

func TestConcurrentNewStoreCreatesExactlyOneStoreIdentity(t *testing.T) {
	directory := t.TempDir()
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			store, err := NewStore(directory)
			if store != nil {
				err = errors.Join(err, store.Close())
			}
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, index.ErrReindexRequired) {
			t.Fatalf("concurrent NewStore() error = %v, want nil or transient ErrReindexRequired", err)
		}
	}
	if successes == 0 {
		t.Fatal("concurrent NewStore() had no successful creator")
	}
	reopened, err := NewStore(directory)
	if err != nil {
		t.Fatalf("NewStore(after concurrent creation) error = %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close(reopened) error = %v", err)
	}

	identity, err := readStoreIdentitySidecar(directory)
	if err != nil {
		t.Fatalf("readStoreIdentitySidecar() error = %v", err)
	}
	database, err := sql.Open("sqlite", inspectionDataSourceName(filepath.Join(directory, databaseName)))
	if err != nil {
		t.Fatalf("sql.Open(identity inspection) error = %v", err)
	}
	defer database.Close()
	identities, err := loadDatabaseIdentities(context.Background(), database)
	if err != nil {
		t.Fatalf("loadDatabaseIdentities() error = %v", err)
	}
	if len(identities) != 1 || identities[0] != identity {
		t.Fatalf("database identities = %v, want singleton sidecar identity", identities)
	}
}

func TestNewStoreRejectsMultipleReservedIdentitiesWithoutMutation(t *testing.T) {
	directory := t.TempDir()
	store, err := NewStore(directory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	databasePath := filepath.Join(directory, databaseName)
	database, err := sql.Open("sqlite", writerDataSourceName(databasePath))
	if err != nil {
		t.Fatalf("sql.Open(corrupt identity fixture) error = %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO repositories(repository_id, active_generation) VALUES (?, NULL)`,
		storeIdentityRepositoryID(strings.Repeat("b", 64)),
	); err != nil {
		_ = database.Close()
		t.Fatalf("insert second identity error = %v", err)
	}
	if _, err := database.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		_ = database.Close()
		t.Fatalf("checkpoint corrupt identity fixture error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("Close(corrupt identity fixture) error = %v", err)
	}
	beforeBytes, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("ReadFile(before) error = %v", err)
	}
	beforeEntries, err := internalDirectoryEntries(directory)
	if err != nil {
		t.Fatalf("ReadDir(before) error = %v", err)
	}

	opened, err := NewStore(directory)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("NewStore(multiple identities) returned store, want nil")
	}
	if !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("NewStore(multiple identities) error = %v, want ErrReindexRequired", err)
	}
	afterBytes, err := os.ReadFile(databasePath)
	if err != nil || !bytes.Equal(afterBytes, beforeBytes) {
		t.Fatalf("multiple-identity database changed: read error %v", err)
	}
	afterEntries, err := internalDirectoryEntries(directory)
	if err != nil || !reflect.DeepEqual(afterEntries, beforeEntries) {
		t.Fatalf("multiple-identity entries = %v, %v; want %v", afterEntries, err, beforeEntries)
	}
}

func internalDirectoryEntries(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names, nil
}

func TestOpenExistingFutureConnectionRejectsDatabasePathSwap(t *testing.T) {
	firstDirectory := t.TempDir()
	firstStore, err := NewStore(firstDirectory)
	if err != nil {
		t.Fatalf("NewStore(first) error = %v", err)
	}
	firstGeneration := internalTestGeneration(t, "first-repository", "first-generation", "first.py", "FIRST = 1")
	if err := firstStore.Replace(context.Background(), firstGeneration); err != nil {
		t.Fatalf("Replace(first) error = %v", err)
	}
	if err := firstStore.checkpoint(context.Background()); err != nil {
		t.Fatalf("checkpoint(first) error = %v", err)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}

	secondDirectory := t.TempDir()
	secondStore, err := NewStore(secondDirectory)
	if err != nil {
		t.Fatalf("NewStore(second) error = %v", err)
	}
	secondGeneration := internalTestGeneration(t, "second-repository", "second-generation", "second.py", "SECOND = 2")
	if err := secondStore.Replace(context.Background(), secondGeneration); err != nil {
		t.Fatalf("Replace(second) error = %v", err)
	}
	if err := secondStore.checkpoint(context.Background()); err != nil {
		t.Fatalf("checkpoint(second) error = %v", err)
	}
	if err := secondStore.Close(); err != nil {
		t.Fatalf("Close(second) error = %v", err)
	}

	opened, err := OpenExisting(firstDirectory)
	if err != nil {
		t.Fatalf("OpenExisting(first) error = %v", err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	opened.db.SetMaxIdleConns(0)
	firstPath := filepath.Join(firstDirectory, databaseName)
	secondPath := filepath.Join(secondDirectory, databaseName)
	if err := os.Rename(firstPath, firstPath+".original"); err != nil {
		t.Fatalf("Rename(first aside) error = %v", err)
	}
	if err := os.Rename(secondPath, firstPath); err != nil {
		t.Fatalf("Rename(second into first path) error = %v", err)
	}

	if _, err := opened.Load(context.Background(), firstGeneration.RepositoryID); !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("Load(first after path swap) error = %v, want identity ErrReindexRequired", err)
	}
}

func TestOpenExistingRejectsDatabaseABAWhileOpening(t *testing.T) {
	firstDirectory, firstPath := internalIdentityStore(t, "first-repository", "first-generation")
	secondDirectory, secondPath := internalIdentityStore(t, "second-repository", "second-generation")
	firstIdentity, err := readStoreIdentitySidecar(firstDirectory)
	if err != nil {
		t.Fatalf("readStoreIdentitySidecar(first) error = %v", err)
	}
	secondIdentity, err := readStoreIdentitySidecar(secondDirectory)
	if err != nil {
		t.Fatalf("readStoreIdentitySidecar(second) error = %v", err)
	}
	firstAside := firstPath + ".original"

	store, err := openExisting(firstDirectory, openExistingHooks{
		beforeOperationalConnection: func(string) error {
			if err := os.Rename(firstPath, firstAside); err != nil {
				return err
			}
			return os.Rename(secondPath, firstPath)
		},
		afterOperationalConnection: func(string) error {
			if err := os.Rename(firstPath, secondPath); err != nil {
				return err
			}
			return os.Rename(firstAside, firstPath)
		},
	})
	if store != nil {
		_ = store.Close()
		t.Fatal("openExisting(ABA) returned store, want nil")
	}
	if !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("openExisting(ABA) error = %v, want ErrReindexRequired", err)
	}
	for _, identity := range []string{firstIdentity, secondIdentity} {
		if strings.Contains(err.Error(), identity) {
			t.Fatalf("openExisting(ABA) error exposes opaque store identity %q", identity)
		}
	}
	if _, err := os.Lstat(firstPath); err != nil {
		t.Fatalf("Lstat(first restored database) error = %v", err)
	}
	if _, err := os.Lstat(secondPath); err != nil {
		t.Fatalf("Lstat(second restored database) error = %v", err)
	}
}

func TestNewStoreRejectsDatabaseABAWhileOpeningWithoutMutation(t *testing.T) {
	firstDirectory, firstPath := internalIdentityStore(t, "first-writer-repository", "first-writer-generation")
	secondDirectory, secondPath := internalIdentityStore(t, "second-writer-repository", "second-writer-generation")
	firstIdentity, err := readStoreIdentitySidecar(firstDirectory)
	if err != nil {
		t.Fatalf("readStoreIdentitySidecar(first) error = %v", err)
	}
	secondIdentity, err := readStoreIdentitySidecar(secondDirectory)
	if err != nil {
		t.Fatalf("readStoreIdentitySidecar(second) error = %v", err)
	}
	firstBefore, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatalf("ReadFile(first before) error = %v", err)
	}
	secondBefore, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatalf("ReadFile(second before) error = %v", err)
	}
	firstEntries, err := internalDirectoryEntries(firstDirectory)
	if err != nil {
		t.Fatalf("ReadDir(first before) error = %v", err)
	}
	secondEntries, err := internalDirectoryEntries(secondDirectory)
	if err != nil {
		t.Fatalf("ReadDir(second before) error = %v", err)
	}
	firstAside := firstPath + ".original"

	store, err := newStore(firstDirectory, storeOpenHooks{
		beforeOperationalConnection: func(string) error {
			if err := os.Rename(firstPath, firstAside); err != nil {
				return err
			}
			return os.Rename(secondPath, firstPath)
		},
		afterOperationalConnection: func(string) error {
			if err := os.Rename(firstPath, secondPath); err != nil {
				return err
			}
			return os.Rename(firstAside, firstPath)
		},
	})
	if store != nil {
		_ = store.Close()
		t.Fatal("newStore(ABA) returned store, want nil")
	}
	if !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("newStore(ABA) error = %v, want ErrReindexRequired", err)
	}
	for _, identity := range []string{firstIdentity, secondIdentity} {
		if strings.Contains(err.Error(), identity) {
			t.Fatalf("newStore(ABA) error exposes opaque store identity %q", identity)
		}
	}
	firstAfter, readErr := os.ReadFile(firstPath)
	if readErr != nil || !bytes.Equal(firstAfter, firstBefore) {
		t.Fatalf("first database changed after writer ABA: read error %v", readErr)
	}
	secondAfter, readErr := os.ReadFile(secondPath)
	if readErr != nil || !bytes.Equal(secondAfter, secondBefore) {
		t.Fatalf("second database changed after writer ABA: read error %v", readErr)
	}
	firstAfterEntries, err := internalDirectoryEntries(firstDirectory)
	if err != nil || !reflect.DeepEqual(firstAfterEntries, firstEntries) {
		t.Fatalf("first entries after ABA = %v, %v; want %v", firstAfterEntries, err, firstEntries)
	}
	secondAfterEntries, err := internalDirectoryEntries(secondDirectory)
	if err != nil || !reflect.DeepEqual(secondAfterEntries, secondEntries) {
		t.Fatalf("second entries after ABA = %v, %v; want %v", secondAfterEntries, err, secondEntries)
	}
}

func TestNewStoreDoesNotRecreateDatabaseDeletedBeforeWriterConnection(t *testing.T) {
	directory, databasePath := internalIdentityStore(t, "deleted-writer-repository", "deleted-writer-generation")
	store, err := newStore(directory, storeOpenHooks{
		beforeOperationalConnection: func(path string) error {
			return os.Remove(path)
		},
	})
	if store != nil {
		_ = store.Close()
		t.Fatal("newStore(deleted database) returned store, want nil")
	}
	if err == nil {
		t.Fatal("newStore(deleted database) error = nil, want failure")
	}
	for _, path := range []string{databasePath, databasePath + "-wal", databasePath + "-shm"} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("Lstat(%s) error = %v, want absent and not recreated", filepath.Base(path), statErr)
		}
	}
}

func internalIdentityStore(t *testing.T, repositoryID, generationID string) (string, string) {
	t.Helper()
	directory := t.TempDir()
	store, err := NewStore(directory)
	if err != nil {
		t.Fatalf("NewStore(%s) error = %v", repositoryID, err)
	}
	generation := internalTestGeneration(t, repositoryID, generationID, repositoryID+".py", "VALUE = 1")
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace(%s) error = %v", repositoryID, err)
	}
	if err := store.checkpoint(context.Background()); err != nil {
		t.Fatalf("checkpoint(%s) error = %v", repositoryID, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(%s) error = %v", repositoryID, err)
	}
	return directory, filepath.Join(directory, databaseName)
}

func internalTestGeneration(t *testing.T, repositoryID, generationID, path, text string) index.Generation {
	t.Helper()
	corpus, err := source.NewCorpus(ingest.ScanPolicyVersion, []source.Chunk{{
		ID:         generationID + "-chunk",
		Text:       text,
		Language:   source.LanguagePython,
		SymbolName: "Fixture",
		Reference:  source.Reference{Path: path, StartLine: 1, EndLine: 1},
	}})
	if err != nil {
		t.Fatalf("NewCorpus() error = %v", err)
	}
	return index.Generation{
		RepositoryID:      repositoryID,
		ID:                generationID,
		CorpusRevision:    corpus.Revision,
		ScanPolicyVersion: corpus.PolicyVersion,
		Chunks:            corpus.Chunks,
		Metric:            index.VectorMetricCosine,
	}
}

type failingCorpusReader struct {
	loadErr  error
	closeErr error
}

func (r failingCorpusReader) Load(context.Context, string) ([]source.Chunk, error) {
	if r.loadErr != nil {
		return nil, r.loadErr
	}
	return []source.Chunk{{ID: "chunk"}}, nil
}

func (r failingCorpusReader) Close() error {
	return r.closeErr
}

func TestLoadAndClosePropagatesReaderCloseError(t *testing.T) {
	closeErr := errors.New("close failure")
	chunks, err := loadAndClose(context.Background(), "repository", failingCorpusReader{closeErr: closeErr})
	if chunks != nil {
		t.Fatalf("loadAndClose() chunks = %#v, want nil after close failure", chunks)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("loadAndClose() error = %v, want close failure", err)
	}
}

func TestLoadAndCloseJoinsLoadAndReaderCloseErrors(t *testing.T) {
	loadErr := errors.New("load failure")
	closeErr := errors.New("close failure")
	chunks, err := loadAndClose(context.Background(), "repository", failingCorpusReader{loadErr: loadErr, closeErr: closeErr})
	if chunks != nil {
		t.Fatalf("loadAndClose() chunks = %#v, want nil", chunks)
	}
	if !errors.Is(err, loadErr) || !errors.Is(err, closeErr) {
		t.Fatalf("loadAndClose() error = %v, want joined load and close failures", err)
	}
}

type failingFinalizationConnection struct {
	*sql.Conn
	committed  bool
	restoreErr error
	closeErr   error
}

func (c *failingFinalizationConnection) ExecContext(ctx context.Context, query string, arguments ...any) (sql.Result, error) {
	if c.committed && query == `PRAGMA busy_timeout=5000` && c.restoreErr != nil {
		return nil, c.restoreErr
	}
	result, err := c.Conn.ExecContext(ctx, query, arguments...)
	if err == nil && query == `COMMIT` {
		c.committed = true
	}
	return result, err
}

func (c *failingFinalizationConnection) Close() error {
	closeErr := c.Conn.Close()
	if c.closeErr != nil {
		return c.closeErr
	}
	return closeErr
}

func TestPublishOnConnectionReportsCommittedFinalizationFailureAndAllowsRetry(t *testing.T) {
	tests := []struct {
		name       string
		stage      index.CleanupStage
		restoreErr error
		closeErr   error
	}{
		{
			name:       "restore",
			stage:      index.CleanupStageConnectionRestore,
			restoreErr: errors.New("PRIVATE_RESTORE_FAILURE_CANARY"),
		},
		{
			name:     "release",
			stage:    index.CleanupStageConnectionRelease,
			closeErr: errors.New("PRIVATE_RELEASE_FAILURE_CANARY"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatalf("NewStore() error = %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			chunk := source.Chunk{
				ID:         "chunk",
				Text:       "PRIVATE_FINALIZATION_SOURCE_CANARY",
				Language:   source.LanguagePython,
				SymbolName: "Finalization",
				Reference:  source.Reference{Path: "private-finalization.py", StartLine: 1, EndLine: 1},
			}
			corpus, err := source.NewCorpus(ingest.ScanPolicyVersion, []source.Chunk{chunk})
			if err != nil {
				t.Fatalf("NewCorpus() error = %v", err)
			}
			generation := index.Generation{
				RepositoryID:      "repository",
				ID:                "generation",
				CorpusRevision:    corpus.Revision,
				ScanPolicyVersion: corpus.PolicyVersion,
				Chunks:            corpus.Chunks,
				Metric:            index.VectorMetricCosine,
			}
			connection, err := store.db.Conn(context.Background())
			if err != nil {
				t.Fatalf("Conn() error = %v", err)
			}
			wrapped := &failingFinalizationConnection{
				Conn:       connection,
				restoreErr: tt.restoreErr,
				closeErr:   tt.closeErr,
			}

			vectors, prepareErr := prepareGenerationVectors(generation)
			if prepareErr != nil {
				t.Fatalf("prepareGenerationVectors() error = %v", prepareErr)
			}
			published, err := publishOnConnection(context.Background(), wrapped, generation, vectors)
			if !published {
				t.Fatalf("publishOnConnection() published = false, want true after commit")
			}
			var committed *index.CommittedCleanupError
			if !errors.As(err, &committed) {
				t.Fatalf("publishOnConnection() error = %T %v, want CommittedCleanupError", err, err)
			}
			if !committed.Published() || committed.Stage() != tt.stage || !errors.Is(err, index.ErrCommittedInfrastructure) {
				t.Fatalf("publishOnConnection() outcome = published %v stage %q error %v", committed.Published(), committed.Stage(), err)
			}
			for _, private := range []string{"PRIVATE_RESTORE_FAILURE_CANARY", "PRIVATE_RELEASE_FAILURE_CANARY", "PRIVATE_FINALIZATION_SOURCE_CANARY", "private-finalization.py"} {
				if errorTreeContains(err, private) {
					t.Fatalf("publishOnConnection() error tree exposes %q", private)
				}
			}
			active, activeErr := store.ActiveGeneration(context.Background(), generation.RepositoryID)
			if activeErr != nil || active != generation.ID {
				t.Fatalf("ActiveGeneration() = %q, %v; want committed %q", active, activeErr, generation.ID)
			}
			if err := store.Replace(context.Background(), generation); err != nil {
				t.Fatalf("Replace(retry committed generation) error = %v", err)
			}
		})
	}
}

func errorTreeContains(err error, value string) bool {
	if err == nil {
		return false
	}
	if strings.Contains(err.Error(), value) {
		return true
	}
	if multiple, ok := err.(interface{ Unwrap() []error }); ok {
		for _, nested := range multiple.Unwrap() {
			if errorTreeContains(nested, value) {
				return true
			}
		}
		return false
	}
	return errorTreeContains(errors.Unwrap(err), value)
}
