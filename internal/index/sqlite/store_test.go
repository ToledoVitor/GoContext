package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ToledoVitor/GoContext/internal/index"
	indexsqlite "github.com/ToledoVitor/GoContext/internal/index/sqlite"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/search/lexical"
	"github.com/ToledoVitor/GoContext/internal/source"
)

var (
	_ lexical.SnapshotLoader = (*indexsqlite.Store)(nil)
	_ lexical.SnapshotLoader = (*indexsqlite.BoundReader)(nil)
)

func TestStorePublishesAndLoadsCorpus(t *testing.T) {
	store := newStore(t)
	corpus := mustCorpus(t, []source.Chunk{sampleChunk("chunk-one", "pkg/app.py", "VALUE = 1")})
	generation := index.Generation{
		RepositoryID:      "owner/repository",
		ID:                "generation-one",
		CorpusRevision:    corpus.Revision,
		ScanPolicyVersion: corpus.PolicyVersion,
		Chunks:            corpus.Chunks,
	}

	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	loaded, err := store.Load(context.Background(), generation.RepositoryID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(loaded, corpus.Chunks) {
		t.Fatalf("Load() = %#v, want %#v", loaded, corpus.Chunks)
	}
	active, err := store.ActiveGeneration(context.Background(), generation.RepositoryID)
	if err != nil {
		t.Fatalf("ActiveGeneration() error = %v", err)
	}
	if active != generation.ID {
		t.Fatalf("ActiveGeneration() = %q, want %q", active, generation.ID)
	}
}

func TestStoreRejectsEmptyGenerationIdentifiers(t *testing.T) {
	store := newStore(t)
	corpus := mustCorpus(t, []source.Chunk{sampleChunk("chunk", "private/canary.py", "PRIVATE_CANARY")})
	generation := index.Generation{
		RepositoryID:      " ",
		ID:                "generation",
		CorpusRevision:    corpus.Revision,
		ScanPolicyVersion: corpus.PolicyVersion,
		Chunks:            corpus.Chunks,
	}

	err := store.Replace(context.Background(), generation)
	if !errors.Is(err, index.ErrInvalidGeneration) {
		t.Fatalf("Replace(empty repository ID) error = %v, want ErrInvalidGeneration", err)
	}
	for _, private := range []string{"private/canary.py", "PRIVATE_CANARY"} {
		if strings.Contains(err.Error(), private) {
			t.Errorf("Replace(empty repository ID) error exposes source %q: %q", private, err)
		}
	}
}

func TestStoreRejectsInvalidChunkProvenance(t *testing.T) {
	store := newStore(t)
	generation := index.Generation{
		RepositoryID:      "repository",
		ID:                "generation",
		CorpusRevision:    "forged-revision",
		ScanPolicyVersion: ingest.ScanPolicyVersion,
		Chunks: []source.Chunk{{
			ID:        "chunk",
			Text:      "PRIVATE_INVALID_REFERENCE_CANARY",
			Reference: source.Reference{Path: "../private-canary.py", StartLine: 1, EndLine: 1},
		}},
	}

	err := store.Replace(context.Background(), generation)
	if !errors.Is(err, index.ErrInvalidGeneration) {
		t.Fatalf("Replace(invalid reference) error = %v, want ErrInvalidGeneration", err)
	}
	for _, private := range []string{"PRIVATE_INVALID_REFERENCE_CANARY", "private-canary.py", "forged-revision"} {
		if strings.Contains(err.Error(), private) {
			t.Errorf("Replace(invalid reference) error exposes source %q: %q", private, err)
		}
	}
}

func TestStoreRequiresCurrentScanPolicy(t *testing.T) {
	store := newStore(t)
	legacy, err := source.NewCorpus("scanner-v3", []source.Chunk{sampleChunk("legacy", "legacy.py", "LEGACY_POLICY_CANARY")})
	if err != nil {
		t.Fatalf("NewCorpus(legacy) error = %v", err)
	}
	generation := index.Generation{
		RepositoryID:      "repository",
		ID:                "legacy-generation",
		CorpusRevision:    legacy.Revision,
		ScanPolicyVersion: legacy.PolicyVersion,
		Chunks:            legacy.Chunks,
	}

	err = store.Replace(context.Background(), generation)
	if !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("Replace(legacy policy) error = %v, want ErrReindexRequired", err)
	}
	if strings.Contains(err.Error(), "LEGACY_POLICY_CANARY") || strings.Contains(err.Error(), "legacy.py") {
		t.Fatalf("Replace(legacy policy) error exposes source: %q", err)
	}
}

func TestStoreRejectsStaleBaseGeneration(t *testing.T) {
	store := newStore(t)
	first := generationFromCorpus(t, "repository", "generation-one", "", []source.Chunk{
		sampleChunk("one", "one.py", "ONE = 1"),
	})
	if err := store.Replace(context.Background(), first); err != nil {
		t.Fatalf("Replace(first) error = %v", err)
	}
	stale := generationFromCorpus(t, "repository", "generation-two", "stale-generation", []source.Chunk{
		sampleChunk("two", "two.py", "TWO = 2"),
	})

	err := store.Replace(context.Background(), stale)
	if !errors.Is(err, index.ErrConcurrentIndex) {
		t.Fatalf("Replace(stale) error = %v, want ErrConcurrentIndex", err)
	}
	active, activeErr := store.ActiveGeneration(context.Background(), first.RepositoryID)
	if activeErr != nil {
		t.Fatalf("ActiveGeneration() error = %v", activeErr)
	}
	if active != first.ID {
		t.Fatalf("ActiveGeneration() = %q, want preserved %q", active, first.ID)
	}
}

func TestStoreRepublishesActiveGenerationIdempotently(t *testing.T) {
	directory := t.TempDir()
	store := openStore(t, directory)
	generation := generationFromCorpus(t, "repository", "stable-generation", "", []source.Chunk{
		sampleChunk("stable", "stable.py", "STABLE = 1"),
	})
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace(first) error = %v", err)
	}
	db := openRawDatabase(t, directory)
	if _, err := db.Exec(`
		CREATE TRIGGER reject_chunk_rewrite
		BEFORE INSERT ON chunks
		BEGIN
			SELECT RAISE(ABORT, 'idempotent publication rewrote chunks');
		END`); err != nil {
		t.Fatalf("create rewrite trigger error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close rewrite-injection database error = %v", err)
	}

	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace(republish) error = %v, want idempotent success", err)
	}
	loaded, err := store.Load(context.Background(), generation.RepositoryID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(loaded, generation.Chunks) {
		t.Fatalf("Load() = %#v, want %#v", loaded, generation.Chunks)
	}
}

func TestStoreRejectsGenerationIDCollisionWithDifferentMetadata(t *testing.T) {
	store := newStore(t)
	first := generationFromCorpus(t, "repository", "same-generation", "", []source.Chunk{
		sampleChunk("first", "first.py", "FIRST = 1"),
	})
	if err := store.Replace(context.Background(), first); err != nil {
		t.Fatalf("Replace(first) error = %v", err)
	}
	different := generationFromCorpus(t, "repository", first.ID, "", []source.Chunk{
		sampleChunk("different", "private-different.py", "PRIVATE_DIFFERENT_CANARY"),
	})

	err := store.Replace(context.Background(), different)
	if !errors.Is(err, index.ErrInvalidGeneration) {
		t.Fatalf("Replace(ID collision) error = %v, want ErrInvalidGeneration", err)
	}
	if strings.Contains(err.Error(), "PRIVATE_DIFFERENT_CANARY") || strings.Contains(err.Error(), "private-different.py") {
		t.Fatalf("Replace(ID collision) error exposes source: %q", err)
	}
}

func TestStoreRejectsInvalidChunksAndCorpusRevision(t *testing.T) {
	valid := generationFromCorpus(t, "repository", "generation", "", []source.Chunk{
		sampleChunk("chunk", "private.py", "PRIVATE_VALIDATION_CANARY"),
	})
	emptyChunkID := valid
	emptyChunkID.Chunks = append([]source.Chunk(nil), valid.Chunks...)
	emptyChunkID.Chunks[0].ID = ""
	duplicateChunkID := valid
	duplicateChunkID.Chunks = append(append([]source.Chunk(nil), valid.Chunks...), valid.Chunks[0])
	forgedRevision := valid
	forgedRevision.CorpusRevision = "forged-private-revision"
	missingPolicy := valid
	missingPolicy.ScanPolicyVersion = ""

	for _, tt := range []struct {
		name       string
		generation index.Generation
	}{
		{name: "empty chunk ID", generation: emptyChunkID},
		{name: "duplicate chunk ID", generation: duplicateChunkID},
		{name: "forged corpus revision", generation: forgedRevision},
		{name: "missing policy", generation: missingPolicy},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t)
			err := store.Replace(context.Background(), tt.generation)
			if !errors.Is(err, index.ErrInvalidGeneration) {
				t.Fatalf("Replace() error = %v, want ErrInvalidGeneration", err)
			}
			for _, private := range []string{"PRIVATE_VALIDATION_CANARY", "private.py", "forged-private-revision"} {
				if strings.Contains(err.Error(), private) {
					t.Errorf("Replace() error exposes %q: %q", private, err)
				}
			}
		})
	}
}

func TestStoreRejectsVectorsUntilEncodingContractIsAvailable(t *testing.T) {
	store := newStore(t)
	generation := generationFromCorpus(t, "repository", "generation", "", []source.Chunk{
		sampleChunk("chunk", "chunk.py", "CHUNK = 1"),
	})
	generation.Vectors = []index.VectorRecord{{ChunkID: "chunk"}}

	err := store.Replace(context.Background(), generation)
	if !errors.Is(err, index.ErrInvalidGeneration) {
		t.Fatalf("Replace(unencoded vectors) error = %v, want ErrInvalidGeneration", err)
	}
}

func TestStorePurgesInactiveGenerationAndTruncatesWAL(t *testing.T) {
	directory := t.TempDir()
	store := openStore(t, directory)
	first := generationFromCorpus(t, "repository", "generation-one", "", []source.Chunk{
		sampleChunk("removed", "removed.py", "REMOVED_PRIVATE_CANARY_7b1d"),
	})
	if err := store.Replace(context.Background(), first); err != nil {
		t.Fatalf("Replace(first) error = %v", err)
	}
	second := generationFromCorpus(t, "repository", "generation-two", first.ID, []source.Chunk{
		sampleChunk("current", "current.py", "CURRENT_VALUE = 2"),
	})
	if err := store.Replace(context.Background(), second); err != nil {
		t.Fatalf("Replace(second) error = %v", err)
	}

	databasePath := filepath.Join(directory, "index-v2.sqlite3")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()
	var generations int
	if err := db.QueryRow(`SELECT count(*) FROM generations WHERE repository_id = ?`, second.RepositoryID).Scan(&generations); err != nil {
		t.Fatalf("count generations error = %v", err)
	}
	if generations != 1 {
		t.Fatalf("stored generations = %d, want only active generation", generations)
	}

	canary := []byte("REMOVED_PRIVATE_CANARY_7b1d")
	for _, path := range []string{databasePath, databasePath + "-wal"} {
		payload, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", filepath.Base(path), err)
		}
		if bytes.Contains(payload, canary) {
			t.Errorf("%s retains removed source canary", filepath.Base(path))
		}
	}
}

func TestStoreBindsActiveCorpus(t *testing.T) {
	store := newStore(t)
	generation := generationFromCorpus(t, "repository", "bound-generation", "", []source.Chunk{
		sampleChunk("bound", "bound.py", "BOUND = 1"),
	})
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	reader, err := store.BindActive(context.Background(), generation.RepositoryID)
	if err != nil {
		t.Fatalf("BindActive() error = %v", err)
	}
	if reader.GenerationID() != generation.ID {
		t.Fatalf("GenerationID() = %q, want %q", reader.GenerationID(), generation.ID)
	}
	loaded, err := reader.Load(context.Background(), generation.RepositoryID)
	if err != nil {
		t.Fatalf("bound Load() error = %v", err)
	}
	if !reflect.DeepEqual(loaded, generation.Chunks) {
		t.Fatalf("bound Load() = %#v, want %#v", loaded, generation.Chunks)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestStoreTransactionFailurePreservesActiveManifest(t *testing.T) {
	directory := t.TempDir()
	store := openStore(t, directory)
	first := generationFromCorpus(t, "repository", "generation-one", "", []source.Chunk{
		sampleChunk("first", "first.py", "FIRST = 1"),
	})
	if err := store.Replace(context.Background(), first); err != nil {
		t.Fatalf("Replace(first) error = %v", err)
	}

	db := openRawDatabase(t, directory)
	if _, err := db.Exec(`
		CREATE TRIGGER reject_manifest_update
		BEFORE UPDATE OF active_generation ON repositories
		WHEN NEW.active_generation = 'generation-two'
		BEGIN
			SELECT RAISE(ABORT, 'forced publication failure');
		END`); err != nil {
		t.Fatalf("create failure trigger error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close failure-injection database error = %v", err)
	}

	second := generationFromCorpus(t, "repository", "generation-two", first.ID, []source.Chunk{
		sampleChunk("second", "private-second.py", "PRIVATE_SECOND_CANARY"),
	})
	err := store.Replace(context.Background(), second)
	if err == nil {
		t.Fatal("Replace(second) error = nil, want injected transaction failure")
	}
	if strings.Contains(err.Error(), "PRIVATE_SECOND_CANARY") || strings.Contains(err.Error(), "private-second.py") {
		t.Fatalf("Replace(second) error exposes source: %q", err)
	}
	active, err := store.ActiveGeneration(context.Background(), first.RepositoryID)
	if err != nil {
		t.Fatalf("ActiveGeneration() error = %v", err)
	}
	if active != first.ID {
		t.Fatalf("ActiveGeneration() = %q, want preserved %q", active, first.ID)
	}
	loaded, err := store.Load(context.Background(), first.RepositoryID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(loaded, first.Chunks) {
		t.Fatalf("Load() = %#v, want preserved %#v", loaded, first.Chunks)
	}

	db = openRawDatabase(t, directory)
	defer db.Close()
	var secondRows int
	if err := db.QueryRow(`SELECT count(*) FROM generations WHERE generation_id = 'generation-two'`).Scan(&secondRows); err != nil {
		t.Fatalf("count rolled-back generation error = %v", err)
	}
	if secondRows != 0 {
		t.Fatalf("rolled-back generation rows = %d, want 0", secondRows)
	}
}

func TestStoreRejectsPersistedStalePolicyAndRevision(t *testing.T) {
	tests := []struct {
		name     string
		mutate   string
		argument any
	}{
		{name: "stale policy", mutate: `UPDATE generations SET scan_policy_version = ?`, argument: "scanner-v3"},
		{name: "forged revision", mutate: `UPDATE generations SET corpus_revision = ?`, argument: "forged-private-revision"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			store := openStore(t, directory)
			generation := generationFromCorpus(t, "repository", "generation", "", []source.Chunk{
				sampleChunk("persisted", "persisted-private.py", "PERSISTED_PRIVATE_CANARY"),
			})
			if err := store.Replace(context.Background(), generation); err != nil {
				t.Fatalf("Replace() error = %v", err)
			}
			db := openRawDatabase(t, directory)
			if _, err := db.Exec(tt.mutate, tt.argument); err != nil {
				t.Fatalf("mutate persisted metadata error = %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close mutation database error = %v", err)
			}

			loaded, err := store.Load(context.Background(), generation.RepositoryID)
			if !errors.Is(err, index.ErrReindexRequired) {
				t.Fatalf("Load() error = %v, want ErrReindexRequired", err)
			}
			if loaded != nil {
				t.Fatalf("Load() = %#v, want nil", loaded)
			}
			for _, private := range []string{"PERSISTED_PRIVATE_CANARY", "persisted-private.py", "forged-private-revision"} {
				if strings.Contains(err.Error(), private) {
					t.Errorf("Load() error exposes %q: %q", private, err)
				}
			}
		})
	}
}

func TestStoreAllowsOnlyOneConcurrentPublisher(t *testing.T) {
	store := newStore(t)
	generations := []index.Generation{
		generationFromCorpus(t, "repository", "generation-one", "", []source.Chunk{sampleChunk("one", "one.py", "ONE = 1")}),
		generationFromCorpus(t, "repository", "generation-two", "", []source.Chunk{sampleChunk("two", "two.py", "TWO = 2")}),
	}
	start := make(chan struct{})
	results := make(chan error, len(generations))
	for _, generation := range generations {
		generation := generation
		go func() {
			<-start
			results <- store.Replace(context.Background(), generation)
		}()
	}
	close(start)

	var succeeded, concurrent int
	for range generations {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, index.ErrConcurrentIndex):
			concurrent++
		default:
			t.Fatalf("concurrent Replace() error = %v", err)
		}
	}
	if succeeded != 1 || concurrent != 1 {
		t.Fatalf("concurrent results = %d success, %d conflict; want 1 and 1", succeeded, concurrent)
	}
}

func TestStoreAllowsOnlyOnePublisherAcrossStoreInstances(t *testing.T) {
	directory := t.TempDir()
	stores := []*indexsqlite.Store{
		openStore(t, directory),
		openStore(t, directory),
	}
	generations := []index.Generation{
		generationFromCorpus(t, "repository", "generation-one", "", []source.Chunk{sampleChunk("one", "one.py", "ONE = 1")}),
		generationFromCorpus(t, "repository", "generation-two", "", []source.Chunk{sampleChunk("two", "two.py", "TWO = 2")}),
	}
	start := make(chan struct{})
	results := make(chan error, len(stores))
	for index := range stores {
		store := stores[index]
		generation := generations[index]
		go func() {
			<-start
			results <- store.Replace(context.Background(), generation)
		}()
	}
	close(start)

	var succeeded, concurrent int
	for range stores {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, index.ErrConcurrentIndex):
			concurrent++
		default:
			t.Fatalf("cross-store Replace() error = %v", err)
		}
	}
	if succeeded != 1 || concurrent != 1 {
		t.Fatalf("cross-store results = %d success, %d conflict; want 1 and 1", succeeded, concurrent)
	}
}

func TestStoreCreatesPrivateVersionedWALDatabase(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "nested", "store")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	store := openStore(t, directory)

	directoryInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("Stat(directory) error = %v", err)
	}
	if got := directoryInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("directory permissions = %04o, want 0700", got)
	}
	databasePath := filepath.Join(directory, "index-v2.sqlite3")
	databaseInfo, err := os.Stat(databasePath)
	if err != nil {
		t.Fatalf("Stat(database) error = %v", err)
	}
	if got := databaseInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("database permissions = %04o, want 0600", got)
	}

	db := openRawDatabase(t, directory)
	defer db.Close()
	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode error = %v", err)
	}
	if strings.ToLower(journalMode) != "wal" {
		t.Errorf("journal_mode = %q, want wal", journalMode)
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("schema version query error = %v", err)
	}
	if version != 1 {
		t.Errorf("schema version = %d, want 1", version)
	}
	for _, table := range []string{"repositories", "generations", "chunks", "vectors"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatalf("schema table query error = %v", err)
		}
		if count != 1 {
			t.Errorf("schema table %q count = %d, want 1", table, count)
		}
	}

	_ = store
}

func TestStoreRejectsUnknownSchemaWithoutMutatingIt(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "index-v2.sqlite3")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER NOT NULL); INSERT INTO schema_version VALUES (2)`); err != nil {
		t.Fatalf("create future schema marker error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close future schema database error = %v", err)
	}

	store, err := indexsqlite.NewStore(directory)
	if store != nil {
		_ = store.Close()
		t.Fatal("NewStore(future schema) returned store, want nil")
	}
	if !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("NewStore(future schema) error = %v, want ErrReindexRequired", err)
	}

	db, err = sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("reopen future schema error = %v", err)
	}
	defer db.Close()
	var createdTables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name IN ('repositories', 'generations', 'chunks', 'vectors')`).Scan(&createdTables); err != nil {
		t.Fatalf("inspect future schema error = %v", err)
	}
	if createdTables != 0 {
		t.Fatalf("future schema gained %d v1 tables, want no mutation", createdTables)
	}
}

func TestStoreRespectsCanceledContext(t *testing.T) {
	store := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	generation := generationFromCorpus(t, "repository", "generation", "", nil)

	if err := store.Replace(ctx, generation); !errors.Is(err, context.Canceled) {
		t.Errorf("Replace(canceled) error = %v, want context.Canceled", err)
	}
	if _, err := store.Load(ctx, generation.RepositoryID); !errors.Is(err, context.Canceled) {
		t.Errorf("Load(canceled) error = %v, want context.Canceled", err)
	}
	if _, err := store.BindActive(ctx, generation.RepositoryID); !errors.Is(err, context.Canceled) {
		t.Errorf("BindActive(canceled) error = %v, want context.Canceled", err)
	}
	if _, err := store.ActiveGeneration(ctx, generation.RepositoryID); !errors.Is(err, context.Canceled) {
		t.Errorf("ActiveGeneration(canceled) error = %v, want context.Canceled", err)
	}
}

func TestStoreCancellationInterruptsLockedPublication(t *testing.T) {
	directory := t.TempDir()
	store := openStore(t, directory)
	release := holdWriteLock(t, directory)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	generation := generationFromCorpus(t, "repository", "generation", "", nil)

	started := time.Now()
	err := store.Replace(ctx, generation)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Replace(locked) error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Replace(locked) cancellation took %v, want under 1s", elapsed)
	}
	if err := release(); err != nil {
		t.Fatalf("release write lock error = %v", err)
	}
	if _, err := store.ActiveGeneration(context.Background(), generation.RepositoryID); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("ActiveGeneration() error = %v, want unpublished ErrNotFound", err)
	}
}

func TestStoreBusyTimeoutAllowsLockedWriterToFinish(t *testing.T) {
	directory := t.TempDir()
	store := openStore(t, directory)
	release := holdWriteLock(t, directory)
	released := make(chan error, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		released <- release()
	}()
	generation := generationFromCorpus(t, "repository", "generation", "", nil)

	started := time.Now()
	err := store.Replace(context.Background(), generation)
	elapsed := time.Since(started)
	if releaseErr := <-released; releaseErr != nil {
		t.Fatalf("release write lock error = %v", releaseErr)
	}
	if err != nil {
		t.Fatalf("Replace(temporarily locked) error = %v", err)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("Replace(temporarily locked) completed in %v, want evidence it waited for lock", elapsed)
	}
}

func TestStoreReportsMissingRepository(t *testing.T) {
	store := newStore(t)
	if _, err := store.Load(context.Background(), "missing"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("Load(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := store.ActiveGeneration(context.Background(), "missing"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("ActiveGeneration(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := store.BindActive(context.Background(), "missing"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("BindActive(missing) error = %v, want ErrNotFound", err)
	}
}

func TestNewStoreRejectsEmptyPathAndExistingFile(t *testing.T) {
	if _, err := indexsqlite.NewStore(""); err == nil {
		t.Fatal("NewStore(empty) error = nil, want error")
	}
	filePath := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(filePath, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := indexsqlite.NewStore(filePath); err == nil {
		t.Fatal("NewStore(file) error = nil, want error")
	}
}

func TestStoreKeepsRepositoriesIsolated(t *testing.T) {
	store := newStore(t)
	first := generationFromCorpus(t, "repository-one", "generation", "", []source.Chunk{sampleChunk("one", "one.py", "ONE = 1")})
	second := generationFromCorpus(t, "../repository-two", "generation", "", []source.Chunk{sampleChunk("two", "two.py", "TWO = 2")})
	for _, generation := range []index.Generation{first, second} {
		if err := store.Replace(context.Background(), generation); err != nil {
			t.Fatalf("Replace() error = %v", err)
		}
	}
	for _, generation := range []index.Generation{first, second} {
		loaded, err := store.Load(context.Background(), generation.RepositoryID)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if !reflect.DeepEqual(loaded, generation.Chunks) {
			t.Fatalf("Load() = %#v, want isolated %#v", loaded, generation.Chunks)
		}
	}
}

func newStore(t *testing.T) *indexsqlite.Store {
	t.Helper()
	return openStore(t, t.TempDir())
}

func openStore(t *testing.T, directory string) *indexsqlite.Store {
	t.Helper()
	store, err := indexsqlite.NewStore(directory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return store
}

func openRawDatabase(t *testing.T, directory string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(directory, "index-v2.sqlite3"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	return db
}

func holdWriteLock(t *testing.T, directory string) func() error {
	t.Helper()
	db := openRawDatabase(t, directory)
	connection, err := db.Conn(context.Background())
	if err != nil {
		_ = db.Close()
		t.Fatalf("open lock connection error = %v", err)
	}
	if _, err := connection.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		_ = connection.Close()
		_ = db.Close()
		t.Fatalf("begin write lock error = %v", err)
	}
	var once sync.Once
	var releaseErr error
	release := func() error {
		once.Do(func() {
			_, rollbackErr := connection.ExecContext(context.Background(), `ROLLBACK`)
			connectionErr := connection.Close()
			databaseErr := db.Close()
			for _, err := range []error{rollbackErr, connectionErr, databaseErr} {
				if err != nil && releaseErr == nil {
					releaseErr = err
				}
			}
		})
		return releaseErr
	}
	t.Cleanup(func() { _ = release() })
	return release
}

func mustCorpus(t *testing.T, chunks []source.Chunk) source.Corpus {
	t.Helper()
	corpus, err := source.NewCorpus(ingest.ScanPolicyVersion, chunks)
	if err != nil {
		t.Fatalf("NewCorpus() error = %v", err)
	}
	return corpus
}

func generationFromCorpus(t *testing.T, repositoryID, generationID, baseGeneration string, chunks []source.Chunk) index.Generation {
	t.Helper()
	corpus := mustCorpus(t, chunks)
	return index.Generation{
		RepositoryID:      repositoryID,
		ID:                generationID,
		BaseGeneration:    baseGeneration,
		CorpusRevision:    corpus.Revision,
		ScanPolicyVersion: corpus.PolicyVersion,
		Chunks:            corpus.Chunks,
	}
}

func sampleChunk(id, path, text string) source.Chunk {
	return source.Chunk{
		ID:         id,
		Text:       text,
		Language:   source.LanguagePython,
		SymbolName: "Value",
		Reference: source.Reference{
			Path:      path,
			StartLine: 3,
			EndLine:   4,
		},
	}
}
