package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
)

func TestRunIndexBuildsRepositorySnapshot(t *testing.T) {
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "app.py", "def load_data():\n    return 1\n")
	writeCLIFile(t, repository, "src/service.ts", "export function saveData() {\n  return true\n}\n")
	writeCLIFile(t, repository, "README.md", "not indexed\n")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"index", "--store", storeDirectory, repository}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(index) code = %d, want 0; stderr = %q", code, stderr.String())
	}
	for _, fragment := range []string{"2 arquivos", "2 símbolos", "2 chunks"} {
		if !strings.Contains(stdout.String(), fragment) {
			t.Errorf("run(index) stdout = %q, want fragment %q", stdout.String(), fragment)
		}
	}

	store, err := localstore.NewStore(storeDirectory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	chunks, err := store.Load(context.Background(), canonicalPath(t, repository))
	if err != nil {
		t.Fatalf("Load(indexed repository) error = %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("Load(indexed repository) returned %d chunks, want 2", len(chunks))
	}
	if chunks[0].Reference.Path != "app.py" || chunks[1].Reference.Path != "src/service.ts" {
		t.Fatalf("chunk paths = [%s %s], want [app.py src/service.ts]", chunks[0].Reference.Path, chunks[1].Reference.Path)
	}
}

func TestRunIndexRejectsInvalidUsage(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run([]string{"index"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("run(index without repository) code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "uso:") {
		t.Fatalf("run(index without repository) stderr = %q, want usage", stderr.String())
	}
}

func TestRunIndexReportsOperationalError(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing")

	code := run([]string{"index", "--store", t.TempDir(), missing}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("run(index missing repository) code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "indexar repositório") {
		t.Fatalf("run(index missing repository) stderr = %q, want operation context", stderr.String())
	}
}

func writeCLIFile(t *testing.T, root, relativePath, content string) {
	t.Helper()
	filePath := filepath.Join(root, relativePath)
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", relativePath, err)
	}
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", relativePath, err)
	}
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs(%q) error = %v", path, err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q) error = %v", path, err)
	}
	return canonical
}
