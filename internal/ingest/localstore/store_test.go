package localstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestStoreAtomicallyReplacesAndLoadsSnapshot(t *testing.T) {
	store, err := localstore.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	first := mustCorpus(t, []source.Chunk{sampleChunk("first", "first.py", "FIRST = 1")})
	if err := store.Replace(context.Background(), "owner/repository", first); err != nil {
		t.Fatalf("first Replace() error = %v", err)
	}
	loaded, err := store.Load(context.Background(), "owner/repository")
	if err != nil {
		t.Fatalf("first Load() error = %v", err)
	}
	if !reflect.DeepEqual(loaded, first.Chunks) {
		t.Fatalf("first Load() = %#v, want %#v", loaded, first.Chunks)
	}

	second := mustCorpus(t, []source.Chunk{sampleChunk("second", "second.ts", "export const SECOND = 2")})
	if err := store.Replace(context.Background(), "owner/repository", second); err != nil {
		t.Fatalf("second Replace() error = %v", err)
	}
	loaded, err = store.Load(context.Background(), "owner/repository")
	if err != nil {
		t.Fatalf("second Load() error = %v", err)
	}
	if !reflect.DeepEqual(loaded, second.Chunks) {
		t.Fatalf("second Load() = %#v, want %#v", loaded, second.Chunks)
	}
}

func TestStoreKeepsRepositoriesIsolated(t *testing.T) {
	store, err := localstore.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	first := mustCorpus(t, []source.Chunk{sampleChunk("first", "first.py", "FIRST = 1")})
	second := mustCorpus(t, []source.Chunk{sampleChunk("second", "second.py", "SECOND = 2")})

	if err := store.Replace(context.Background(), "repository-one", first); err != nil {
		t.Fatalf("Replace(repository-one) error = %v", err)
	}
	if err := store.Replace(context.Background(), "../repository-two", second); err != nil {
		t.Fatalf("Replace(repository-two) error = %v", err)
	}

	loadedFirst, err := store.Load(context.Background(), "repository-one")
	if err != nil {
		t.Fatalf("Load(repository-one) error = %v", err)
	}
	loadedSecond, err := store.Load(context.Background(), "../repository-two")
	if err != nil {
		t.Fatalf("Load(repository-two) error = %v", err)
	}
	if !reflect.DeepEqual(loadedFirst, first.Chunks) || !reflect.DeepEqual(loadedSecond, second.Chunks) {
		t.Fatalf("isolated loads = %#v and %#v, want %#v and %#v", loadedFirst, loadedSecond, first.Chunks, second.Chunks)
	}
}

func TestStoreReportsMissingAndRejectsInvalidInput(t *testing.T) {
	store, err := localstore.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	if _, err := store.Load(context.Background(), "missing"); !errors.Is(err, localstore.ErrNotFound) {
		t.Fatalf("Load(missing) error = %v, want ErrNotFound", err)
	} else if !strings.Contains(err.Error(), "load repository snapshot") {
		t.Errorf("Load(missing) error = %q, want operation context", err)
	}
	if err := store.Replace(context.Background(), "", source.Corpus{}); !errors.Is(err, localstore.ErrInvalidRepositoryID) {
		t.Fatalf("Replace(empty ID) error = %v, want ErrInvalidRepositoryID", err)
	}
	invalid := source.Corpus{PolicyVersion: ingest.ScanPolicyVersion, Revision: "invalid", Chunks: []source.Chunk{{ID: "invalid"}}}
	if err := store.Replace(context.Background(), "repo", invalid); !errors.Is(err, localstore.ErrInvalidChunk) {
		t.Fatalf("Replace(invalid chunk) error = %v, want ErrInvalidChunk", err)
	} else if !strings.Contains(err.Error(), "replace repository snapshot") {
		t.Errorf("Replace(invalid chunk) error = %q, want operation context", err)
	}
	duplicate := sampleChunk("same", "app.py", "A = 1")
	invalid = source.Corpus{PolicyVersion: ingest.ScanPolicyVersion, Revision: "invalid", Chunks: []source.Chunk{duplicate, duplicate}}
	if err := store.Replace(context.Background(), "repo", invalid); !errors.Is(err, localstore.ErrInvalidChunk) {
		t.Fatalf("Replace(duplicate chunks) error = %v, want ErrInvalidChunk", err)
	}
}

func TestStoreRespectsCancellation(t *testing.T) {
	store, err := localstore.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := store.Replace(ctx, "repo", source.Corpus{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Replace(canceled) error = %v, want context.Canceled", err)
	}
	if _, err := store.Load(ctx, "repo"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load(canceled) error = %v, want context.Canceled", err)
	}
}

func TestNewStoreRejectsEmptyPathAndExistingFile(t *testing.T) {
	if _, err := localstore.NewStore(""); err == nil {
		t.Fatal("NewStore(empty) error = nil, want error")
	}

	filePath := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(filePath, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := localstore.NewStore(filePath); err == nil {
		t.Fatal("NewStore(file) error = nil, want error")
	}
}

func TestNewStoreCreatesNestedDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "nested", "store")
	store, err := localstore.NewStore(directory)
	if err != nil {
		t.Fatalf("NewStore(nested) error = %v", err)
	}
	if err := store.Replace(context.Background(), "repo", mustCorpus(t, nil)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
}

func TestStoreRejectsLegacySnapshotWithoutReturningCanary(t *testing.T) {
	directory := t.TempDir()
	store, err := localstore.NewStore(directory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if err := store.Replace(context.Background(), "repo", mustCorpus(t, nil)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("snapshot entries = %d, want 1", len(entries))
	}
	legacy := `{"version":1,"repository_id":"repo","chunks":[{"id":"legacy","text":"LEGACY_CANARY","language":"python","reference":{"Path":"secret.py","StartLine":1,"EndLine":1}}]}`
	if err := os.WriteFile(filepath.Join(directory, entries[0].Name()), []byte(legacy), 0o600); err != nil {
		t.Fatalf("WriteFile(legacy) error = %v", err)
	}

	loaded, err := store.Load(context.Background(), "repo")
	if !errors.Is(err, localstore.ErrReindexRequired) {
		t.Fatalf("Load(legacy) error = %v, want ErrReindexRequired", err)
	}
	if loaded != nil {
		t.Fatalf("Load(legacy) = %#v, want nil", loaded)
	}
	if strings.Contains(err.Error(), "LEGACY_CANARY") || strings.Contains(err.Error(), "secret.py") {
		t.Fatalf("Load(legacy) error exposes legacy data: %q", err)
	}
}

func TestStoreRejectsCorpusWhosePolicyOrRevisionWasNotValidated(t *testing.T) {
	store, err := localstore.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	chunk := sampleChunk("chunk", "app.py", "VALUE = 1")
	for _, corpus := range []source.Corpus{
		{PolicyVersion: "scanner-v1", Revision: "legacy", Chunks: []source.Chunk{chunk}},
		{PolicyVersion: ingest.ScanPolicyVersion, Revision: "forged", Chunks: []source.Chunk{chunk}},
	} {
		if err := store.Replace(context.Background(), "repo", corpus); !errors.Is(err, localstore.ErrInvalidChunk) {
			t.Errorf("Replace(%#v) error = %v, want ErrInvalidChunk", corpus, err)
		}
	}
}

func TestStoreLoadRejectsPersistedV2WithOldPolicyOrForgedRevision(t *testing.T) {
	chunk := sampleChunk("persisted-canary", "persisted-canary.py", "PERSISTED_V2_CANARY = 1")
	legacyCorpus, err := source.NewCorpus("scanner-v2", []source.Chunk{chunk})
	if err != nil {
		t.Fatalf("NewCorpus(old policy) error = %v", err)
	}
	currentCorpus := mustCorpus(t, []source.Chunk{chunk})
	tests := []struct {
		name          string
		policyVersion string
		revision      string
	}{
		{name: "old policy", policyVersion: legacyCorpus.PolicyVersion, revision: legacyCorpus.Revision},
		{name: "forged revision", policyVersion: currentCorpus.PolicyVersion, revision: "forged-revision"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			store, err := localstore.NewStore(directory)
			if err != nil {
				t.Fatalf("NewStore() error = %v", err)
			}
			if err := store.Replace(context.Background(), "repo", mustCorpus(t, nil)); err != nil {
				t.Fatalf("Replace() error = %v", err)
			}
			entries, err := os.ReadDir(directory)
			if err != nil {
				t.Fatalf("ReadDir() error = %v", err)
			}
			payload, err := json.Marshal(struct {
				Version        int            `json:"version"`
				RepositoryID   string         `json:"repository_id"`
				PolicyVersion  string         `json:"policy_version"`
				CorpusRevision string         `json:"corpus_revision"`
				Chunks         []source.Chunk `json:"chunks"`
			}{
				Version:        2,
				RepositoryID:   "repo",
				PolicyVersion:  tt.policyVersion,
				CorpusRevision: tt.revision,
				Chunks:         []source.Chunk{chunk},
			})
			if err != nil {
				t.Fatalf("Marshal(v2 snapshot) error = %v", err)
			}
			if err := os.WriteFile(filepath.Join(directory, entries[0].Name()), payload, 0o600); err != nil {
				t.Fatalf("WriteFile(v2 snapshot) error = %v", err)
			}

			loaded, err := store.Load(context.Background(), "repo")
			if !errors.Is(err, localstore.ErrReindexRequired) {
				t.Fatalf("Load(v2 %s) error = %v, want ErrReindexRequired", tt.name, err)
			}
			if loaded != nil {
				t.Fatalf("Load(v2 %s) = %#v, want nil", tt.name, loaded)
			}
			for _, canary := range []string{"PERSISTED_V2_CANARY", "persisted-canary.py", "forged-revision"} {
				if strings.Contains(err.Error(), canary) {
					t.Errorf("Load(v2 %s) error exposes %q: %q", tt.name, canary, err)
				}
			}
		})
	}
}

func sampleChunk(id, path, text string) source.Chunk {
	return source.Chunk{
		ID:        id,
		Text:      text,
		Language:  source.LanguagePython,
		Reference: source.Reference{Path: path, StartLine: 1, EndLine: 1},
	}
}

func mustCorpus(t *testing.T, chunks []source.Chunk) source.Corpus {
	t.Helper()
	corpus, err := source.NewCorpus(ingest.ScanPolicyVersion, chunks)
	if err != nil {
		t.Fatalf("NewCorpus() error = %v", err)
	}
	return corpus
}
