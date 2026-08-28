package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/index"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestWriterInitializesPrivateStoreIdentityAndReadOnlyRequiresIt(t *testing.T) {
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

	legacyReader, err := OpenExisting(directory)
	if legacyReader != nil {
		_ = legacyReader.Close()
		t.Fatal("OpenExisting(legacy store) returned store, want nil")
	}
	if !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("OpenExisting(legacy store) error = %v, want ErrReindexRequired", err)
	}
	identityPath := filepath.Join(directory, "index-v2.identity.json")
	if _, err := os.Lstat(identityPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenExisting(legacy store) identity sidecar error = %v, want absent", err)
	}

	writer, err := NewStore(directory)
	if err != nil {
		t.Fatalf("NewStore(legacy store) error = %v", err)
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
