package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/index"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestOpenExistingPinsValidatedDatabaseIdentityAcrossPathSwap(t *testing.T) {
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

	loaded, err := opened.Load(context.Background(), firstGeneration.RepositoryID)
	if err != nil {
		t.Fatalf("Load(first after path swap) error = %v", err)
	}
	if !reflect.DeepEqual(loaded, firstGeneration.Chunks) {
		t.Fatalf("Load(first after path swap) = %#v, want pinned %#v", loaded, firstGeneration.Chunks)
	}
	if _, err := opened.Load(context.Background(), secondGeneration.RepositoryID); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("Load(second after path swap) error = %v, want pinned database ErrNotFound", err)
	}
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
