package localstore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestStoreAtomicallyReplacesAndLoadsSnapshot(t *testing.T) {
	store, err := localstore.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	first := []source.Chunk{sampleChunk("first", "first.py", "FIRST = 1")}
	if err := store.Replace(context.Background(), "owner/repository", first); err != nil {
		t.Fatalf("first Replace() error = %v", err)
	}
	loaded, err := store.Load(context.Background(), "owner/repository")
	if err != nil {
		t.Fatalf("first Load() error = %v", err)
	}
	if !reflect.DeepEqual(loaded, first) {
		t.Fatalf("first Load() = %#v, want %#v", loaded, first)
	}

	second := []source.Chunk{sampleChunk("second", "second.ts", "export const SECOND = 2")}
	if err := store.Replace(context.Background(), "owner/repository", second); err != nil {
		t.Fatalf("second Replace() error = %v", err)
	}
	loaded, err = store.Load(context.Background(), "owner/repository")
	if err != nil {
		t.Fatalf("second Load() error = %v", err)
	}
	if !reflect.DeepEqual(loaded, second) {
		t.Fatalf("second Load() = %#v, want %#v", loaded, second)
	}
}

func TestStoreKeepsRepositoriesIsolated(t *testing.T) {
	store, err := localstore.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	first := []source.Chunk{sampleChunk("first", "first.py", "FIRST = 1")}
	second := []source.Chunk{sampleChunk("second", "second.py", "SECOND = 2")}

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
	if !reflect.DeepEqual(loadedFirst, first) || !reflect.DeepEqual(loadedSecond, second) {
		t.Fatalf("isolated loads = %#v and %#v, want %#v and %#v", loadedFirst, loadedSecond, first, second)
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
	if err := store.Replace(context.Background(), "", nil); !errors.Is(err, localstore.ErrInvalidRepositoryID) {
		t.Fatalf("Replace(empty ID) error = %v, want ErrInvalidRepositoryID", err)
	}
	if err := store.Replace(context.Background(), "repo", []source.Chunk{{ID: "invalid"}}); !errors.Is(err, localstore.ErrInvalidChunk) {
		t.Fatalf("Replace(invalid chunk) error = %v, want ErrInvalidChunk", err)
	} else if !strings.Contains(err.Error(), "replace repository snapshot") {
		t.Errorf("Replace(invalid chunk) error = %q, want operation context", err)
	}
	duplicate := sampleChunk("same", "app.py", "A = 1")
	if err := store.Replace(context.Background(), "repo", []source.Chunk{duplicate, duplicate}); !errors.Is(err, localstore.ErrInvalidChunk) {
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

	if err := store.Replace(ctx, "repo", nil); !errors.Is(err, context.Canceled) {
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
	if err := store.Replace(context.Background(), "repo", nil); err != nil {
		t.Fatalf("Replace() error = %v", err)
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
