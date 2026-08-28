package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	indexsqlite "github.com/ToledoVitor/GoContext/internal/index/sqlite"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
)

func TestRunIndexDefaultBuildsRepositorySnapshot(t *testing.T) {
	clearEmbeddingEnvironment(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)
	t.Setenv(embeddingBaseURLEnv, server.URL+"/v1")
	t.Setenv(embeddingModelEnv, "inert-model")
	t.Setenv(embeddingAPIKeyEnv, "INERT_KEY_CANARY")
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
	if got := requests.Load(); got != 0 {
		t.Fatalf("default index embedding requests = %d, want 0", got)
	}
	if _, err := os.Stat(filepath.Join(storeDirectory, "index-v2.sqlite3")); !os.IsNotExist(err) {
		t.Fatalf("default index SQLite stat error = %v, want not exist", err)
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

func TestRunIndexSQLiteOffPublishesRollbackReadyGenerationWithoutNetwork(t *testing.T) {
	clearEmbeddingEnvironment(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)
	t.Setenv(embeddingBaseURLEnv, server.URL+"/v1")
	t.Setenv(embeddingModelEnv, "inert-model")
	t.Setenv(embeddingAPIKeyEnv, "INERT_KEY_CANARY")
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "app.py", "def persisted_value():\n    return 1\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", repository}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(index sqlite off) code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "indexado: 1 arquivos, 1 símbolos, 1 chunks\n"; got != want {
		t.Fatalf("run(index sqlite off) stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("run(index sqlite off) stderr = %q, want empty", stderr.String())
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("run(index sqlite off) embedding requests = %d, want 0", got)
	}
	for _, path := range []string{
		filepath.Join(storeDirectory, "index-v2.sqlite3"),
		rollbackMarkerPath(storeDirectory, canonicalPath(t, repository)),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%q) error = %v", filepath.Base(path), err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode(%q) = %o, want 600", filepath.Base(path), info.Mode().Perm())
		}
	}
	store, err := localstore.NewStore(storeDirectory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	chunks, err := store.Load(context.Background(), canonicalPath(t, repository))
	if err != nil || len(chunks) != 1 {
		t.Fatalf("rollback snapshot Load() = %d chunks, error %v; want 1", len(chunks), err)
	}
	markerPayload, err := os.ReadFile(rollbackMarkerPath(storeDirectory, canonicalPath(t, repository)))
	if err != nil {
		t.Fatalf("ReadFile(rollback marker) error = %v", err)
	}
	var marker rollbackMarker
	if err := json.Unmarshal(markerPayload, &marker); err != nil {
		t.Fatalf("Unmarshal(rollback marker) error = %v", err)
	}
	corpus := mustCLICorpus(t, chunks)
	if marker.Version != rollbackMarkerSchemaVersion ||
		marker.RepositoryHash != repositoryHash(canonicalPath(t, repository)) ||
		marker.ScanPolicy != ingest.ScanPolicyVersion ||
		marker.CorpusRevision != corpus.Revision || marker.ActiveGeneration == "" {
		t.Fatalf("rollback marker = %#v, want bound current metadata", marker)
	}
	if bytes.Contains(markerPayload, []byte(canonicalPath(t, repository))) ||
		bytes.Contains(markerPayload, []byte("persisted_value")) ||
		bytes.Contains(markerPayload, []byte("INERT_KEY_CANARY")) {
		t.Fatalf("rollback marker exposes repository/source/key: %q", markerPayload)
	}
	sqliteStore, err := indexsqlite.OpenExisting(storeDirectory)
	if err != nil {
		t.Fatalf("OpenExisting() error = %v", err)
	}
	active, err := sqliteStore.ActiveGeneration(context.Background(), canonicalPath(t, repository))
	if err != nil {
		t.Fatalf("ActiveGeneration() error = %v", err)
	}
	if err := sqliteStore.Close(); err != nil {
		t.Fatalf("Close(SQLite) error = %v", err)
	}
	if marker.ActiveGeneration != active {
		t.Fatalf("rollback marker generation = %q, want active %q", marker.ActiveGeneration, active)
	}
}

func TestRunIndexSnapshotAfterSQLiteInvalidatesRollbackAndKeepsDefaultAuthoritative(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "value.py", "def old_value():\n    return 1\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", repository}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(index sqlite) code = %d; stderr = %q", code, stderr.String())
	}
	markerPath := rollbackMarkerPath(storeDirectory, canonicalPath(t, repository))
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("Stat(rollback marker before snapshot) error = %v", err)
	}

	writeCLIFile(t, repository, "value.py", "def new_value():\n    return 2\n")
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"index", "--store", storeDirectory, repository}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(index snapshot) code = %d; stderr = %q", code, stderr.String())
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("Stat(rollback marker after snapshot) error = %v, want not exist", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"search", "--store", storeDirectory, repository, "new", "value"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(search default snapshot) code = %d; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "new_value") || strings.Contains(stdout.String(), "old_value") {
		t.Fatalf("run(search default snapshot) stdout = %q, want new snapshot only", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"search", "--store", storeDirectory, "--index-backend", "auto", repository, "old", "value"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(search auto stale SQLite) code = %d; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "old_value") || strings.Contains(stdout.String(), "new_value") {
		t.Fatalf("run(search auto stale SQLite) stdout = %q, want old opt-in SQLite generation", stdout.String())
	}
}

func TestRunIndexSQLiteCommitWithSnapshotFailureLeavesRollbackUnready(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "committed.py", "def committed_value():\n    return 1\n")
	repositoryID := canonicalPath(t, repository)
	snapshotCollision := filepath.Join(storeDirectory, repositoryHash(repositoryID)+".json")
	if err := os.MkdirAll(filepath.Join(snapshotCollision, "non-empty"), 0o700); err != nil {
		t.Fatalf("MkdirAll(snapshot collision) error = %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", repository}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(index rollback snapshot failure) code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "indexado: 1 arquivos, 1 símbolos, 1 chunks\n"; got != want {
		t.Fatalf("run(index rollback snapshot failure) stdout = %q, want committed report %q", got, want)
	}
	if !strings.Contains(stderr.String(), "rollback não está pronto") || strings.Contains(stderr.String(), repositoryID) {
		t.Fatalf("run(index rollback snapshot failure) stderr = %q, want fixed rollback category", stderr.String())
	}
	if _, err := os.Stat(rollbackMarkerPath(storeDirectory, repositoryID)); !os.IsNotExist(err) {
		t.Fatalf("Stat(rollback marker) error = %v, want not ready", err)
	}
	store, err := indexsqlite.OpenExisting(storeDirectory)
	if err != nil {
		t.Fatalf("OpenExisting() error = %v", err)
	}
	if _, err := store.ActiveGeneration(context.Background(), repositoryID); err != nil {
		t.Fatalf("ActiveGeneration() error = %v, want committed SQLite generation", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestRunIndexSQLiteCommitWithMarkerFailureLeavesRollbackUnready(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository := t.TempDir()
	repositoryID := canonicalPath(t, repository)
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "marker.py", "def marker_value():\n    return 1\n")
	markerCollision := rollbackMarkerPath(storeDirectory, repositoryID)
	if err := os.MkdirAll(markerCollision, 0o700); err != nil {
		t.Fatalf("MkdirAll(marker collision) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(markerCollision, "non-empty"), []byte("MARKER_FAILURE_CANARY"), 0o600); err != nil {
		t.Fatalf("WriteFile(marker collision) error = %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", repository}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(index marker failure) code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "indexado: 1 arquivos, 1 símbolos, 1 chunks\n"; got != want {
		t.Fatalf("run(index marker failure) stdout = %q, want committed report %q", got, want)
	}
	if !strings.Contains(stderr.String(), "rollback não está pronto") || strings.Contains(stderr.String(), "MARKER_FAILURE_CANARY") {
		t.Fatalf("run(index marker failure) stderr = %q, want fixed rollback category", stderr.String())
	}
	info, err := os.Stat(markerCollision)
	if err != nil || !info.IsDir() {
		t.Fatalf("Stat(marker collision) = %v, error %v; want no valid marker file", info, err)
	}
	store, err := indexsqlite.OpenExisting(storeDirectory)
	if err != nil {
		t.Fatalf("OpenExisting() error = %v", err)
	}
	if _, err := store.ActiveGeneration(context.Background(), repositoryID); err != nil {
		t.Fatalf("ActiveGeneration() error = %v, want committed SQLite generation", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestRunIndexSQLiteCommittedCleanupOutcomeStillPublishesRollbackCompanion(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository := t.TempDir()
	repositoryID := canonicalPath(t, repository)
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "generation.py", "def first_generation():\n    return 1\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", repository}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(index first generation) code = %d; stderr = %q", code, stderr.String())
	}
	readerStore, err := indexsqlite.OpenExisting(storeDirectory)
	if err != nil {
		t.Fatalf("OpenExisting(reader) error = %v", err)
	}
	reader, err := readerStore.BindActive(context.Background(), repositoryID)
	if err != nil {
		_ = readerStore.Close()
		t.Fatalf("BindActive() error = %v", err)
	}
	t.Cleanup(func() {
		_ = reader.Close()
		_ = readerStore.Close()
	})
	if _, err := reader.Load(context.Background(), repositoryID); err != nil {
		t.Fatalf("reader.Load(first) error = %v", err)
	}

	writeCLIFile(t, repository, "generation.py", "def second_generation():\n    return 2\n")
	stdout.Reset()
	stderr.Reset()
	code := run([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", repository}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(index with pinned old reader) code = %d, want committed maintenance 1; stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "indexado: 1 arquivos, 1 símbolos, 1 chunks\n"; got != want {
		t.Fatalf("run(index with pinned old reader) stdout = %q, want %q", got, want)
	}
	if !strings.Contains(stderr.String(), "manutenção incompleta") {
		t.Fatalf("run(index with pinned old reader) stderr = %q, want fixed maintenance outcome", stderr.String())
	}
	markerPayload, err := os.ReadFile(rollbackMarkerPath(storeDirectory, repositoryID))
	if err != nil {
		t.Fatalf("ReadFile(rollback marker) error = %v", err)
	}
	var marker rollbackMarker
	if err := json.Unmarshal(markerPayload, &marker); err != nil || marker.ActiveGeneration == "" {
		t.Fatalf("rollback marker = %#v, error %v; want ready published generation", marker, err)
	}
	pinnedChunks, err := reader.Load(context.Background(), repositoryID)
	if err != nil {
		t.Fatalf("reader.Load(pinned after publish) error = %v", err)
	}
	if len(pinnedChunks) != 1 || !strings.Contains(pinnedChunks[0].Text, "first_generation") {
		t.Fatalf("pinned chunks = %#v, want immutable first generation", pinnedChunks)
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
