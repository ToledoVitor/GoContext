package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ToledoVitor/GoContext/internal/embedding"
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
		Metric:            index.VectorMetricCosine,
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

func TestStorePersistsAndValidatesVectorMetric(t *testing.T) {
	directory := t.TempDir()
	store := openStore(t, directory)
	generation := generationFromCorpus(t, "repository", "generation", "", []source.Chunk{
		sampleChunk("chunk", "metric.py", "METRIC = 1"),
	})
	generation.Profile = &embedding.Profile{Fingerprint: "profile-fingerprint", Model: "model"}
	generation.Dimensions = 3
	generation.Vectors = []index.VectorRecord{{ChunkID: "chunk", Values: embedding.Vector{1, 0, 0}}}
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	db := openRawDatabase(t, directory)
	var metric string
	if err := db.QueryRow(`SELECT metric FROM generations WHERE repository_id = ? AND generation_id = ?`, generation.RepositoryID, generation.ID).Scan(&metric); err != nil {
		t.Fatalf("query metric error = %v", err)
	}
	if metric != string(index.VectorMetricCosine) {
		t.Fatalf("stored metric = %q, want %q", metric, index.VectorMetricCosine)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw database error = %v", err)
	}

	reopened := openStore(t, directory)
	if err := reopened.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace(idempotent after reopen) error = %v", err)
	}
}

func TestStoreRejectsPersistedVectorMetricMismatchAfterReopen(t *testing.T) {
	directory := t.TempDir()
	store := openStore(t, directory)
	generation := generationFromCorpus(t, "repository", "generation", "", []source.Chunk{
		sampleChunk("chunk", "private-metric.py", "PRIVATE_METRIC_CANARY"),
	})
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	db := openRawDatabase(t, directory)
	if _, err := db.Exec(`PRAGMA ignore_check_constraints=ON; UPDATE generations SET metric = 'dot-product'`); err != nil {
		t.Fatalf("corrupt metric error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw database error = %v", err)
	}

	reopened := openStore(t, directory)
	_, err := reopened.Load(context.Background(), generation.RepositoryID)
	if !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("Load(metric mismatch) error = %v, want ErrReindexRequired", err)
	}
	for _, private := range []string{"PRIVATE_METRIC_CANARY", "private-metric.py"} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("Load(metric mismatch) error exposes source %q: %v", private, err)
		}
	}
}

func TestStoreRejectsInconsistentVectorMetadata(t *testing.T) {
	valid := generationFromCorpus(t, "repository", "generation", "", []source.Chunk{
		sampleChunk("chunk", "metadata.py", "METADATA = 1"),
	})
	missingMetric := valid
	missingMetric.Metric = ""
	unsupportedMetric := valid
	unsupportedMetric.Metric = index.VectorMetric("dot-product")
	dimensionsWithoutProfile := valid
	dimensionsWithoutProfile.Dimensions = 3
	profileWithoutDimensions := valid
	profileWithoutDimensions.Profile = &embedding.Profile{Fingerprint: "fingerprint", Model: "model"}
	emptyProfile := valid
	emptyProfile.Profile = &embedding.Profile{}
	emptyProfile.Dimensions = 3

	for _, tt := range []struct {
		name       string
		generation index.Generation
	}{
		{name: "missing metric", generation: missingMetric},
		{name: "unsupported metric", generation: unsupportedMetric},
		{name: "dimensions without profile", generation: dimensionsWithoutProfile},
		{name: "profile without dimensions", generation: profileWithoutDimensions},
		{name: "empty profile", generation: emptyProfile},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t)
			if err := store.Replace(context.Background(), tt.generation); !errors.Is(err, index.ErrInvalidGeneration) {
				t.Fatalf("Replace(inconsistent vector metadata) error = %v, want ErrInvalidGeneration", err)
			}
		})
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
		Metric:            index.VectorMetricCosine,
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
		Metric:            index.VectorMetricCosine,
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
		Metric:            index.VectorMetricCosine,
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

func TestStoreRejectsActiveGenerationWithChangedCanonicalContent(t *testing.T) {
	store := newStore(t)
	first := generationFromCorpus(t, "repository", "same-generation", "", []source.Chunk{
		sampleChunk("first", "private-first.py", "PRIVATE_FIRST_CANARY"),
		sampleChunk("second", "private-second.py", "PRIVATE_SECOND_CANARY"),
	})
	if err := store.Replace(context.Background(), first); err != nil {
		t.Fatalf("Replace(first) error = %v", err)
	}

	mutations := []struct {
		name   string
		mutate func([]source.Chunk)
	}{
		{name: "text", mutate: func(chunks []source.Chunk) { chunks[0].Text = "PRIVATE_CHANGED_TEXT_CANARY" }},
		{name: "reference path", mutate: func(chunks []source.Chunk) { chunks[0].Reference.Path = "private-changed.py" }},
		{name: "reference start", mutate: func(chunks []source.Chunk) { chunks[0].Reference.StartLine++ }},
		{name: "reference end", mutate: func(chunks []source.Chunk) { chunks[0].Reference.EndLine++ }},
		{name: "language", mutate: func(chunks []source.Chunk) { chunks[0].Language = source.LanguageTypeScript }},
		{name: "symbol", mutate: func(chunks []source.Chunk) { chunks[0].SymbolName = "PrivateChangedSymbol" }},
		{name: "order", mutate: func(chunks []source.Chunk) { chunks[0], chunks[1] = chunks[1], chunks[0] }},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			changedChunks := append([]source.Chunk(nil), first.Chunks...)
			tt.mutate(changedChunks)
			changed := generationFromCorpus(t, first.RepositoryID, first.ID, first.BaseGeneration, changedChunks)
			if changed.CorpusRevision != first.CorpusRevision {
				t.Fatalf("mutation changed corpus revision = %q, want stable %q", changed.CorpusRevision, first.CorpusRevision)
			}

			err := store.Replace(context.Background(), changed)
			if !errors.Is(err, index.ErrInvalidGeneration) {
				t.Fatalf("Replace(changed canonical content) error = %v, want ErrInvalidGeneration", err)
			}
			for _, private := range []string{"PRIVATE_CHANGED_TEXT_CANARY", "private-changed.py", "PrivateChangedSymbol"} {
				if strings.Contains(err.Error(), private) {
					t.Fatalf("Replace(changed canonical content) error exposes source %q: %v", private, err)
				}
			}
		})
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

func TestStoreReportsCommittedPublicationWhenPurgeFails(t *testing.T) {
	directory := t.TempDir()
	store := openStore(t, directory)
	first := generationFromCorpus(t, "repository", "generation-one", "", []source.Chunk{
		sampleChunk("first", "private-first.py", "PRIVATE_FIRST_CANARY"),
	})
	if err := store.Replace(context.Background(), first); err != nil {
		t.Fatalf("Replace(first) error = %v", err)
	}

	db := openRawDatabase(t, directory)
	if _, err := db.Exec(`
		CREATE TRIGGER fail_inactive_generation_purge
		BEFORE DELETE ON generations
		WHEN OLD.generation_id = 'generation-one'
		BEGIN
			SELECT RAISE(ABORT, 'PRIVATE_CLEANUP_FAILURE_CANARY');
		END`); err != nil {
		t.Fatalf("create cleanup failure trigger error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close cleanup failure database error = %v", err)
	}

	second := generationFromCorpus(t, first.RepositoryID, "generation-two", first.ID, []source.Chunk{
		sampleChunk("second", "private-second.py", "PRIVATE_SECOND_CANARY"),
	})
	err := store.Replace(context.Background(), second)
	var committed *index.CommittedCleanupError
	if !errors.As(err, &committed) {
		t.Fatalf("Replace(second) error = %T %v, want CommittedCleanupError", err, err)
	}
	if !errors.Is(err, index.ErrCleanupIncomplete) || !committed.Published() {
		t.Fatalf("Replace(second) error = %v, want published cleanup-incomplete outcome", err)
	}
	if committed.Stage() != index.CleanupStagePurge {
		t.Fatalf("Replace(second) cleanup stage = %q, want %q", committed.Stage(), index.CleanupStagePurge)
	}
	if errorTreeContainsForTest(err, "PRIVATE_CLEANUP_FAILURE_CANARY") || strings.Contains(fmt.Sprintf("%+v", err), "PRIVATE_CLEANUP_FAILURE_CANARY") {
		t.Fatalf("Replace(second) error tree exposes cleanup trigger content: %v", err)
	}
	var sqliteCause interface{ Code() int }
	if errors.As(err, &sqliteCause) {
		t.Fatalf("Replace(second) exposes raw SQLite cause with code %d", sqliteCause.Code())
	}
	active, activeErr := store.ActiveGeneration(context.Background(), first.RepositoryID)
	if activeErr != nil {
		t.Fatalf("ActiveGeneration() error = %v", activeErr)
	}
	if active != second.ID {
		t.Fatalf("ActiveGeneration() = %q, want committed %q", active, second.ID)
	}
	loaded, loadErr := store.Load(context.Background(), second.RepositoryID)
	if loadErr != nil {
		t.Fatalf("Load(committed) error = %v", loadErr)
	}
	if !reflect.DeepEqual(loaded, second.Chunks) {
		t.Fatalf("Load(committed) = %#v, want %#v", loaded, second.Chunks)
	}

	db = openRawDatabase(t, directory)
	if _, err := db.Exec(`DROP TRIGGER fail_inactive_generation_purge`); err != nil {
		t.Fatalf("drop cleanup failure trigger error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close cleanup retry database error = %v", err)
	}
	if err := store.Replace(context.Background(), second); err != nil {
		t.Fatalf("Replace(retry committed generation) error = %v", err)
	}

	db = openRawDatabase(t, directory)
	defer db.Close()
	var generationCount int
	if err := db.QueryRow(`SELECT count(*) FROM generations WHERE repository_id = ?`, second.RepositoryID).Scan(&generationCount); err != nil {
		t.Fatalf("count generations after retry error = %v", err)
	}
	if generationCount != 1 {
		t.Fatalf("generation count after retry = %d, want 1", generationCount)
	}
}

func TestStoreReportsCommittedPublicationWhenCheckpointIsBlocked(t *testing.T) {
	directory := t.TempDir()
	store := openStore(t, directory)
	first := generationFromCorpus(t, "repository", "generation-one", "", []source.Chunk{
		sampleChunk("first", "first.py", "FIRST = 1"),
	})
	if err := store.Replace(context.Background(), first); err != nil {
		t.Fatalf("Replace(first) error = %v", err)
	}
	reader, err := store.BindActive(context.Background(), first.RepositoryID)
	if err != nil {
		t.Fatalf("BindActive(first) error = %v", err)
	}

	second := generationFromCorpus(t, first.RepositoryID, "generation-two", first.ID, []source.Chunk{
		sampleChunk("second", "second.py", "SECOND = 2"),
	})
	err = store.Replace(context.Background(), second)
	var committed *index.CommittedCleanupError
	if !errors.As(err, &committed) {
		_ = reader.Close()
		t.Fatalf("Replace(second) error = %T %v, want CommittedCleanupError", err, err)
	}
	if committed.Stage() != index.CleanupStageCheckpoint || !committed.Published() {
		_ = reader.Close()
		t.Fatalf("Replace(second) outcome = stage %q published %v, want checkpoint/true", committed.Stage(), committed.Published())
	}
	active, activeErr := store.ActiveGeneration(context.Background(), first.RepositoryID)
	if activeErr != nil {
		_ = reader.Close()
		t.Fatalf("ActiveGeneration() error = %v", activeErr)
	}
	if active != second.ID {
		_ = reader.Close()
		t.Fatalf("ActiveGeneration() = %q, want committed %q", active, second.ID)
	}
	pinned, pinnedErr := reader.Load(context.Background(), first.RepositoryID)
	if pinnedErr != nil {
		_ = reader.Close()
		t.Fatalf("bound Load(first) error = %v", pinnedErr)
	}
	if !reflect.DeepEqual(pinned, first.Chunks) {
		_ = reader.Close()
		t.Fatalf("bound Load(first) = %#v, want %#v", pinned, first.Chunks)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close(reader) error = %v", err)
	}
	if err := store.Replace(context.Background(), second); err != nil {
		t.Fatalf("Replace(retry committed generation) error = %v", err)
	}
}

func TestStoreRejectsPersistedStalePolicyRevisionAndContent(t *testing.T) {
	tests := []struct {
		name     string
		mutate   string
		argument any
	}{
		{name: "stale policy", mutate: `UPDATE generations SET scan_policy_version = ?`, argument: "scanner-v3"},
		{name: "forged revision", mutate: `UPDATE generations SET corpus_revision = ?`, argument: "forged-private-revision"},
		{name: "changed canonical content", mutate: `UPDATE chunks SET text = ?`, argument: "PERSISTED_CHANGED_PRIVATE_CANARY"},
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
			for _, private := range []string{"PERSISTED_PRIVATE_CANARY", "PERSISTED_CHANGED_PRIVATE_CANARY", "persisted-private.py", "forged-private-revision"} {
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
	if err := os.Chmod(directory, 0o750); err != nil {
		t.Fatalf("Chmod(future schema directory) error = %v", err)
	}
	beforeBytes, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("ReadFile(before) error = %v", err)
	}
	beforeInfo, err := os.Stat(databasePath)
	if err != nil {
		t.Fatalf("Stat(before) error = %v", err)
	}
	beforeEntries := directoryEntryNames(t, directory)
	beforeDirectoryInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("Stat(directory before) error = %v", err)
	}

	store, err := indexsqlite.NewStore(directory)
	if store != nil {
		_ = store.Close()
		t.Fatal("NewStore(future schema) returned store, want nil")
	}
	if !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("NewStore(future schema) error = %v, want ErrReindexRequired", err)
	}
	afterBytes, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("ReadFile(after) error = %v", err)
	}
	afterInfo, err := os.Stat(databasePath)
	if err != nil {
		t.Fatalf("Stat(after) error = %v", err)
	}
	if !bytes.Equal(afterBytes, beforeBytes) {
		t.Error("future schema database bytes changed during rejection")
	}
	if afterInfo.ModTime() != beforeInfo.ModTime() || afterInfo.Mode() != beforeInfo.Mode() || afterInfo.Size() != beforeInfo.Size() {
		t.Errorf("future schema metadata changed: before=%v after=%v", beforeInfo, afterInfo)
	}
	if afterEntries := directoryEntryNames(t, directory); !reflect.DeepEqual(afterEntries, beforeEntries) {
		t.Errorf("future schema directory entries = %v, want unchanged %v", afterEntries, beforeEntries)
	}
	afterDirectoryInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("Stat(directory after) error = %v", err)
	}
	if afterDirectoryInfo.Mode() != beforeDirectoryInfo.Mode() || afterDirectoryInfo.ModTime() != beforeDirectoryInfo.ModTime() {
		t.Errorf("future schema directory metadata changed: before=%v after=%v", beforeDirectoryInfo, afterDirectoryInfo)
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

func TestStoreRejectsUnknownWALSchemaWithoutMutatingDatabaseFamily(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "index-v2.sqlite3")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
		PRAGMA journal_mode=WAL;
		PRAGMA wal_autocheckpoint=0;
		CREATE TABLE schema_version(version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (2)`); err != nil {
		_ = db.Close()
		t.Fatalf("create future WAL schema error = %v", err)
	}

	type fileSnapshot struct {
		content []byte
		mode    os.FileMode
		modTime time.Time
	}
	before := make(map[string]fileSnapshot)
	for _, path := range []string{databasePath, databasePath + "-wal", databasePath + "-shm"} {
		content, err := os.ReadFile(path)
		if err != nil {
			_ = db.Close()
			t.Fatalf("ReadFile(%s) error = %v", filepath.Base(path), err)
		}
		info, err := os.Stat(path)
		if err != nil {
			_ = db.Close()
			t.Fatalf("Stat(%s) error = %v", filepath.Base(path), err)
		}
		before[path] = fileSnapshot{content: content, mode: info.Mode(), modTime: info.ModTime()}
	}
	beforeEntries := directoryEntryNames(t, directory)

	store, err := indexsqlite.NewStore(directory)
	if store != nil {
		_ = store.Close()
		_ = db.Close()
		t.Fatal("NewStore(future WAL schema) returned store, want nil")
	}
	if !errors.Is(err, index.ErrReindexRequired) {
		_ = db.Close()
		t.Fatalf("NewStore(future WAL schema) error = %v, want ErrReindexRequired", err)
	}
	if afterEntries := directoryEntryNames(t, directory); !reflect.DeepEqual(afterEntries, beforeEntries) {
		_ = db.Close()
		t.Errorf("future WAL schema directory entries = %v, want unchanged %v", afterEntries, beforeEntries)
	}
	for path, want := range before {
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			_ = db.Close()
			t.Fatalf("ReadFile(%s after) error = %v", filepath.Base(path), readErr)
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			_ = db.Close()
			t.Fatalf("Stat(%s after) error = %v", filepath.Base(path), statErr)
		}
		if !bytes.Equal(content, want.content) || info.Mode() != want.mode || info.ModTime() != want.modTime {
			t.Errorf("future WAL schema file changed: %s", filepath.Base(path))
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close future WAL schema database error = %v", err)
	}
}

func TestStoreRejectsMalformedV1SchemaWithoutMutatingIt(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "index-v2.sqlite3")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE schema_version(version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES (1);
		CREATE TABLE repositories(repository_id TEXT PRIMARY KEY, active_generation TEXT);
		CREATE TABLE generations(repository_id TEXT, generation_id TEXT);
		CREATE TABLE chunks(repository_id TEXT, generation_id TEXT, chunk_id TEXT);
		CREATE TABLE vectors(repository_id TEXT, generation_id TEXT, chunk_id TEXT);`); err != nil {
		t.Fatalf("create malformed v1 error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close malformed v1 database error = %v", err)
	}
	beforeBytes, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("ReadFile(before) error = %v", err)
	}
	beforeInfo, err := os.Stat(databasePath)
	if err != nil {
		t.Fatalf("Stat(before) error = %v", err)
	}
	beforeEntries := directoryEntryNames(t, directory)

	store, err := indexsqlite.NewStore(directory)
	if store != nil {
		_ = store.Close()
		t.Fatal("NewStore(malformed v1) returned store, want nil")
	}
	if !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("NewStore(malformed v1) error = %v, want ErrReindexRequired", err)
	}
	afterBytes, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("ReadFile(after) error = %v", err)
	}
	afterInfo, err := os.Stat(databasePath)
	if err != nil {
		t.Fatalf("Stat(after) error = %v", err)
	}
	if !bytes.Equal(afterBytes, beforeBytes) {
		t.Error("malformed v1 database bytes changed during rejection")
	}
	if afterInfo.ModTime() != beforeInfo.ModTime() || afterInfo.Mode() != beforeInfo.Mode() || afterInfo.Size() != beforeInfo.Size() {
		t.Errorf("malformed v1 metadata changed: before=%v after=%v", beforeInfo, afterInfo)
	}
	if afterEntries := directoryEntryNames(t, directory); !reflect.DeepEqual(afterEntries, beforeEntries) {
		t.Errorf("malformed v1 directory entries = %v, want unchanged %v", afterEntries, beforeEntries)
	}
}

func TestStoreRejectsV1SchemaWithMissingRelationalConstraints(t *testing.T) {
	tests := []struct {
		name        string
		chunksTable string
	}{
		{
			name: "foreign key",
			chunksTable: `CREATE TABLE chunks (
				repository_id TEXT NOT NULL,
				generation_id TEXT NOT NULL,
				chunk_id TEXT NOT NULL,
				ordinal INTEGER NOT NULL,
				text TEXT NOT NULL,
				language TEXT NOT NULL,
				symbol_name TEXT NOT NULL,
				path TEXT NOT NULL,
				start_line INTEGER NOT NULL,
				end_line INTEGER NOT NULL,
				PRIMARY KEY (repository_id, generation_id, chunk_id),
				UNIQUE (repository_id, generation_id, ordinal)
			)`,
		},
		{
			name: "ordinal uniqueness",
			chunksTable: `CREATE TABLE chunks (
				repository_id TEXT NOT NULL,
				generation_id TEXT NOT NULL,
				chunk_id TEXT NOT NULL,
				ordinal INTEGER NOT NULL,
				text TEXT NOT NULL,
				language TEXT NOT NULL,
				symbol_name TEXT NOT NULL,
				path TEXT NOT NULL,
				start_line INTEGER NOT NULL,
				end_line INTEGER NOT NULL,
				PRIMARY KEY (repository_id, generation_id, chunk_id),
				FOREIGN KEY (repository_id, generation_id)
					REFERENCES generations(repository_id, generation_id) ON DELETE CASCADE
			)`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			store := openStore(t, directory)
			if err := store.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			db := openRawDatabase(t, directory)
			if _, err := db.Exec(`
				PRAGMA foreign_keys=OFF;
				PRAGMA legacy_alter_table=ON;
				BEGIN;
				ALTER TABLE chunks RENAME TO old_chunks;
				` + tt.chunksTable + `;
				DROP TABLE old_chunks;
				COMMIT;`); err != nil {
				t.Fatalf("replace chunks schema error = %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close malformed schema database error = %v", err)
			}

			store, err := indexsqlite.NewStore(directory)
			if store != nil {
				_ = store.Close()
				t.Fatal("NewStore(missing constraint) returned store, want nil")
			}
			if !errors.Is(err, index.ErrReindexRequired) {
				t.Fatalf("NewStore(missing constraint) error = %v, want ErrReindexRequired", err)
			}
		})
	}
}

func TestStoreRejectsMaliciousSchemaIndexNameWithoutExecutingIt(t *testing.T) {
	directory := t.TempDir()
	store := openStore(t, directory)
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	db := openRawDatabase(t, directory)
	if _, err := db.Exec(`
		PRAGMA foreign_keys=OFF;
		PRAGMA legacy_alter_table=ON;
		BEGIN;
		DROP TABLE vectors;
		ALTER TABLE chunks RENAME TO old_chunks;
		CREATE TABLE chunks (
			repository_id TEXT NOT NULL,
			generation_id TEXT NOT NULL,
			chunk_id TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			text TEXT NOT NULL,
			language TEXT NOT NULL,
			symbol_name TEXT NOT NULL,
			path TEXT NOT NULL,
			start_line INTEGER NOT NULL,
			end_line INTEGER NOT NULL,
			PRIMARY KEY (repository_id, generation_id, chunk_id),
			FOREIGN KEY (repository_id, generation_id)
				REFERENCES generations(repository_id, generation_id) ON DELETE CASCADE
		);
		DROP TABLE old_chunks;
		CREATE TABLE vectors (
			repository_id TEXT NOT NULL,
			generation_id TEXT NOT NULL,
			chunk_id TEXT NOT NULL,
			encoding_version INTEGER NOT NULL,
			dimensions INTEGER NOT NULL,
			values_blob BLOB NOT NULL,
			PRIMARY KEY (repository_id, generation_id, chunk_id),
			FOREIGN KEY (repository_id, generation_id, chunk_id)
				REFERENCES chunks(repository_id, generation_id, chunk_id) ON DELETE CASCADE
		);
		CREATE INDEX safe_expected_columns
			ON chunks(repository_id, generation_id, ordinal);
		CREATE UNIQUE INDEX "malicious""); PRAGMA index_info(""safe_expected_columns"
			ON chunks(repository_id);
		COMMIT;`); err != nil {
		_ = db.Close()
		t.Fatalf("create malicious index schema error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close malicious index database error = %v", err)
	}

	store, err := indexsqlite.NewStore(directory)
	if store != nil {
		_ = store.Close()
		t.Fatal("NewStore(malicious index schema) returned store, want nil")
	}
	if !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("NewStore(malicious index schema) error = %v, want ErrReindexRequired", err)
	}

	db = openRawDatabase(t, directory)
	defer db.Close()
	var marker, injectedTables int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&marker); err != nil {
		t.Fatalf("read schema marker after rejection error = %v", err)
	}
	if marker != 1 {
		t.Fatalf("schema marker after rejection = %d, want 1", marker)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name = 'injected'`).Scan(&injectedTables); err != nil {
		t.Fatalf("inspect injected schema objects error = %v", err)
	}
	if injectedTables != 0 {
		t.Fatalf("injected schema objects = %d, want 0", injectedTables)
	}
}

func TestStoreRejectsMalformedV1DefaultsAndMetricConstraint(t *testing.T) {
	tests := []struct {
		name   string
		mutate string
		canary string
	}{
		{
			name: "dimensions default",
			mutate: `UPDATE sqlite_schema
				SET sql = replace(
					sql,
					'dimensions INTEGER NOT NULL DEFAULT 0',
					'dimensions INTEGER NOT NULL DEFAULT ''PRIVATE_DEFAULT_DDL_CANARY'''
				)
				WHERE type = 'table' AND name = 'generations'`,
			canary: "PRIVATE_DEFAULT_DDL_CANARY",
		},
		{
			name: "metric check",
			mutate: `UPDATE sqlite_schema
				SET sql = replace(
					sql,
					'metric TEXT NOT NULL CHECK (metric = ''cosine'')',
					'metric TEXT NOT NULL CHECK (metric = ''cosine'' OR metric = ''PRIVATE_METRIC_DDL_CANARY'')'
				)
				WHERE type = 'table' AND name = 'generations'`,
			canary: "PRIVATE_METRIC_DDL_CANARY",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			store := openStore(t, directory)
			if err := store.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			db := openRawDatabase(t, directory)
			if _, err := db.Exec(`PRAGMA writable_schema=ON`); err != nil {
				_ = db.Close()
				t.Fatalf("enable writable_schema error = %v", err)
			}
			result, err := db.Exec(tt.mutate)
			if err != nil {
				_ = db.Close()
				t.Fatalf("mutate generations DDL error = %v", err)
			}
			updated, err := result.RowsAffected()
			if err != nil || updated != 1 {
				_ = db.Close()
				t.Fatalf("mutate generations DDL rows = %d, error = %v; want 1", updated, err)
			}
			if _, err := db.Exec(`PRAGMA schema_version=2; PRAGMA writable_schema=OFF`); err != nil {
				_ = db.Close()
				t.Fatalf("finish writable_schema mutation error = %v", err)
			}
			var storedDDL string
			if err := db.QueryRow(`SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = 'generations'`).Scan(&storedDDL); err != nil {
				_ = db.Close()
				t.Fatalf("read mutated generations DDL error = %v", err)
			}
			if !strings.Contains(storedDDL, tt.canary) {
				_ = db.Close()
				t.Fatalf("mutated generations DDL does not contain canary")
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close malformed DDL database error = %v", err)
			}

			store, err = indexsqlite.NewStore(directory)
			if store != nil {
				_ = store.Close()
				t.Fatal("NewStore(malformed generations DDL) returned store, want nil")
			}
			if !errors.Is(err, index.ErrReindexRequired) {
				t.Fatalf("NewStore(malformed generations DDL) error = %v, want ErrReindexRequired", err)
			}
			if errorTreeContainsForTest(err, tt.canary) {
				t.Fatalf("NewStore(malformed generations DDL) error exposes DDL canary %q", tt.canary)
			}
		})
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

func TestOpenExistingNeverCreatesMissingStore(t *testing.T) {
	root := t.TempDir()
	missingDirectory := filepath.Join(root, "missing")
	if _, err := indexsqlite.OpenExisting(missingDirectory); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("OpenExisting(missing directory) error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(missingDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(missing directory) error = %v, want not exist", err)
	}

	emptyDirectory := t.TempDir()
	if _, err := indexsqlite.OpenExisting(emptyDirectory); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("OpenExisting(empty directory) error = %v, want ErrNotFound", err)
	}
	if entries := directoryEntryNames(t, emptyDirectory); len(entries) != 0 {
		t.Fatalf("OpenExisting(empty directory) created entries %v", entries)
	}
}

func TestOpenExistingRejectsDatabaseSymlinksAndNonRegularFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link creation is not reliably available to unprivileged Windows tests")
	}
	t.Run("valid symlink", func(t *testing.T) {
		targetDirectory := t.TempDir()
		store := openStore(t, targetDirectory)
		if err := store.Close(); err != nil {
			t.Fatalf("Close(target) error = %v", err)
		}
		directory := t.TempDir()
		if err := os.Symlink(filepath.Join(targetDirectory, "index-v2.sqlite3"), filepath.Join(directory, "index-v2.sqlite3")); err != nil {
			t.Fatalf("Symlink(database) error = %v", err)
		}
		if _, err := indexsqlite.OpenExisting(directory); err == nil || errors.Is(err, index.ErrNotFound) {
			t.Fatalf("OpenExisting(database symlink) error = %v, want fatal non-absence error", err)
		}
	})
	t.Run("dangling symlink", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.Symlink(filepath.Join(directory, "missing-target"), filepath.Join(directory, "index-v2.sqlite3")); err != nil {
			t.Fatalf("Symlink(dangling database) error = %v", err)
		}
		if _, err := indexsqlite.OpenExisting(directory); err == nil || errors.Is(err, index.ErrNotFound) {
			t.Fatalf("OpenExisting(dangling database symlink) error = %v, want fatal non-absence error", err)
		}
	})
	t.Run("directory at database path", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.Mkdir(filepath.Join(directory, "index-v2.sqlite3"), 0o700); err != nil {
			t.Fatalf("Mkdir(database path) error = %v", err)
		}
		if _, err := indexsqlite.OpenExisting(directory); err == nil || errors.Is(err, index.ErrNotFound) {
			t.Fatalf("OpenExisting(non-regular database) error = %v, want fatal non-absence error", err)
		}
	})
}

func TestOpenExistingRejectsUnsafePrivateDatabaseModeWithoutMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX private mode bits are not meaningful on Windows")
	}
	directory := t.TempDir()
	store := openStore(t, directory)
	if err := store.Close(); err != nil {
		t.Fatalf("Close(created) error = %v", err)
	}
	databasePath := filepath.Join(directory, "index-v2.sqlite3")
	if err := os.Chmod(databasePath, 0o640); err != nil {
		t.Fatalf("Chmod(database) error = %v", err)
	}

	if _, err := indexsqlite.OpenExisting(directory); err == nil || errors.Is(err, index.ErrNotFound) {
		t.Fatalf("OpenExisting(unsafe mode) error = %v, want fatal non-absence error", err)
	}
	info, err := os.Lstat(databasePath)
	if err != nil {
		t.Fatalf("Lstat(database) error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("OpenExisting changed database mode to %#o, want %#o", got, 0o640)
	}
}

func TestOpenExistingRejectsUnknownSchemaWithoutMutation(t *testing.T) {
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
		t.Fatalf("Close(future schema) error = %v", err)
	}
	if err := os.Chmod(databasePath, 0o600); err != nil {
		t.Fatalf("Chmod(future schema) error = %v", err)
	}
	beforeBytes, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("ReadFile(before) error = %v", err)
	}
	beforeInfo, err := os.Lstat(databasePath)
	if err != nil {
		t.Fatalf("Lstat(before) error = %v", err)
	}

	store, err := indexsqlite.OpenExisting(directory)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenExisting(future schema) returned store, want nil")
	}
	if !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("OpenExisting(future schema) error = %v, want ErrReindexRequired", err)
	}
	afterBytes, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatalf("ReadFile(after) error = %v", err)
	}
	afterInfo, err := os.Lstat(databasePath)
	if err != nil {
		t.Fatalf("Lstat(after) error = %v", err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) || beforeInfo.Mode() != afterInfo.Mode() || beforeInfo.Size() != afterInfo.Size() || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatalf("OpenExisting(future schema) mutated database: before=%v after=%v", beforeInfo, afterInfo)
	}
}

func TestNewStoreRejectsExistingDatabaseSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic-link creation is not reliably available to unprivileged Windows tests")
	}
	targetDirectory := t.TempDir()
	target := openStore(t, targetDirectory)
	if err := target.Close(); err != nil {
		t.Fatalf("Close(target) error = %v", err)
	}
	targetPath := filepath.Join(targetDirectory, "index-v2.sqlite3")
	before, err := os.Lstat(targetPath)
	if err != nil {
		t.Fatalf("Lstat(target before) error = %v", err)
	}
	directory := t.TempDir()
	if err := os.Symlink(targetPath, filepath.Join(directory, "index-v2.sqlite3")); err != nil {
		t.Fatalf("Symlink(database) error = %v", err)
	}

	if _, err := indexsqlite.NewStore(directory); err == nil {
		t.Fatal("NewStore(database symlink) error = nil, want rejection")
	}
	after, err := os.Lstat(targetPath)
	if err != nil {
		t.Fatalf("Lstat(target after) error = %v", err)
	}
	if before.Mode() != after.Mode() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("NewStore(database symlink) mutated target metadata: before=%v after=%v", before, after)
	}
}

func TestOpenExistingLoadsPersistedStore(t *testing.T) {
	directory := t.TempDir()
	created := openStore(t, directory)
	generation := generationFromCorpus(t, "repository", "generation", "", []source.Chunk{
		sampleChunk("chunk", "persisted.py", "VALUE = 1"),
	})
	if err := created.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if err := created.Close(); err != nil {
		t.Fatalf("Close(created) error = %v", err)
	}
	databasePath := filepath.Join(directory, "index-v2.sqlite3")
	beforeInfo, err := os.Stat(databasePath)
	if err != nil {
		t.Fatalf("Stat(database before OpenExisting) error = %v", err)
	}

	reopened, err := indexsqlite.OpenExisting(directory)
	if err != nil {
		t.Fatalf("OpenExisting() error = %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("Close(reopened) error = %v", err)
		}
	})
	loaded, err := reopened.Load(context.Background(), generation.RepositoryID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(loaded, generation.Chunks) {
		t.Fatalf("Load() = %#v, want %#v", loaded, generation.Chunks)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close(reopened) error = %v", err)
	}
	afterInfo, err := os.Stat(databasePath)
	if err != nil {
		t.Fatalf("Stat(database after OpenExisting) error = %v", err)
	}
	if afterInfo.Size() != beforeInfo.Size() || !afterInfo.ModTime().Equal(beforeInfo.ModTime()) || afterInfo.Mode() != beforeInfo.Mode() {
		t.Fatalf("OpenExisting changed database metadata: before=%v after=%v", beforeInfo, afterInfo)
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

func directoryEntryNames(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names
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
		Metric:            index.VectorMetricCosine,
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

func errorTreeContainsForTest(err error, value string) bool {
	if err == nil {
		return false
	}
	if strings.Contains(err.Error(), value) {
		return true
	}
	if multiple, ok := err.(interface{ Unwrap() []error }); ok {
		for _, nested := range multiple.Unwrap() {
			if errorTreeContainsForTest(nested, value) {
				return true
			}
		}
		return false
	}
	return errorTreeContainsForTest(errors.Unwrap(err), value)
}
