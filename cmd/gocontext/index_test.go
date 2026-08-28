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

func TestRunIndexDefaultBuildsRepositorySnapshot(t *testing.T) {
	clearEmbeddingEnvironment(t)
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
	if got, want := stdout.String(), "indexado: 2 arquivos, 2 símbolos, 2 chunks\n"; got != want {
		t.Fatalf("run(index) stdout = %q, want %q", got, want)
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

	explicitStore := t.TempDir()
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"index", "--index-backend", "snapshot", "--store", explicitStore, repository}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(index explicit snapshot) code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "indexado: 2 arquivos, 2 símbolos, 2 chunks\n"; got != want {
		t.Fatalf("run(index explicit snapshot) stdout = %q, want %q", got, want)
	}
}

func TestRunIndexRejectsInvalidUsage(t *testing.T) {
	clearEmbeddingEnvironment(t)
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
	clearEmbeddingEnvironment(t)
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

func TestRunIndexAbortsBeforeCreatingSnapshotWhenScanPolicyFails(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "valid.py", "print('must not be stored')\n")
	writeCLIFile(t, repository, "bad\u200b.py", "print('invalid path')\n")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"index", "--store", storeDirectory, repository}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(index invalid path) code = %d, want 1", code)
	}
	entries, err := os.ReadDir(storeDirectory)
	if err != nil {
		t.Fatalf("ReadDir(store) error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("store entries = %v, want none after scan failure", entries)
	}
	if strings.Contains(stderr.String(), "bad\u200b.py") || strings.Contains(stderr.String(), "invalid path") {
		t.Fatalf("run(index invalid path) stderr exposes source data: %q", stderr.String())
	}
}

func TestRunIndexRejectsInvalidSemanticConfigurationWithoutEcho(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run([]string{"index", "--semantic=bad\x1b[31mMODE_CANARY", repository}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("run(index invalid semantic mode) code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid semantic mode") {
		t.Fatalf("run(index invalid semantic mode) stderr = %q, want fixed configuration category", stderr.String())
	}
	if strings.Contains(stderr.String(), "MODE_CANARY") || strings.ContainsRune(stderr.String(), '\x1b') {
		t.Fatalf("run(index invalid semantic mode) stderr exposes raw value: %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("run(index invalid semantic mode) stdout = %q, want empty", stdout.String())
	}
}

func TestRunIndexRejectsUnavailableSQLiteWithoutSnapshotFallback(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "app.py", "print('must not be indexed')\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", repository}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("run(index sqlite not wired) code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not wired yet") {
		t.Fatalf("run(index sqlite not wired) stderr = %q, want fixed operational category", stderr.String())
	}
	entries, err := os.ReadDir(storeDirectory)
	if err != nil {
		t.Fatalf("ReadDir(store) error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("store entries = %v, want none when sqlite is not wired", entries)
	}
}

func TestRunIndexSemanticSnapshotRequiresSQLite(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run([]string{
		"index",
		"--semantic", "preferred",
		"--embedding-base-url", "https://api.example.test/v1",
		"--embedding-model", "example-model",
		repository,
	}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("run(index semantic snapshot) code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--index-backend sqlite") {
		t.Fatalf("run(index semantic snapshot) stderr = %q, want sqlite requirement", stderr.String())
	}
}

func TestRunIndexRejectsAPIKeyFlagWithoutEcho(t *testing.T) {
	clearEmbeddingEnvironment(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run([]string{"index", "--embedding-api-key=KEY_CLI_CANARY\x1b[31m", t.TempDir()}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("run(index API key flag) code = %d, want 2", code)
	}
	if strings.Contains(stderr.String(), "KEY_CLI_CANARY") || strings.ContainsRune(stderr.String(), '\x1b') {
		t.Fatalf("run(index API key flag) stderr exposes key: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "API_KEY") {
		t.Fatalf("run(index API key flag) stderr exposes environment mechanism: %q", stderr.String())
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
