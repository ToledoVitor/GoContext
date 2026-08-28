package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	indexdomain "github.com/ToledoVitor/GoContext/internal/index"
	indexsqlite "github.com/ToledoVitor/GoContext/internal/index/sqlite"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
	searchdomain "github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestRunSearchDefaultPrintsRankedCitationAndText(t *testing.T) {
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
	store, err := localstore.NewStore(storeDirectory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	chunks := []source.Chunk{
		{
			ID:         "best",
			Text:       "def load_user():\n    return user",
			Language:   source.LanguagePython,
			SymbolName: "LoadUser",
			Reference:  source.Reference{Path: "src/user.py", StartLine: 3, EndLine: 4},
		},
		{
			ID:         "partial",
			Text:       "def load():\n    pass",
			Language:   source.LanguagePython,
			SymbolName: "Load",
			Reference:  source.Reference{Path: "src/load.py", StartLine: 1, EndLine: 2},
		},
	}
	if err := store.Replace(context.Background(), canonicalPath(t, repository), mustCLICorpus(t, chunks)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{
		"search", "--store", storeDirectory, "--limit", "1", repository, "load", "user",
	}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(search) code = %d, want 0; stderr = %q", code, stderr.String())
	}
	wantOutput := "0.950 src/user.py:3-4 LoadUser\ndef load_user():\n    return user\n"
	if got := stdout.String(); got != wantOutput {
		t.Fatalf("run(search) stdout = %q, want %q", got, wantOutput)
	}
	if strings.Contains(stdout.String(), "partial") || strings.Contains(stdout.String(), "src/load.py") {
		t.Errorf("run(search) stdout = %q, want limit 1", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"search", "--index-backend", "snapshot", "--store", storeDirectory, "--limit", "1", repository, "load", "user",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(search explicit snapshot) code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if got := stdout.String(); got != wantOutput {
		t.Fatalf("run(search explicit snapshot) stdout = %q, want %q", got, wantOutput)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("default search embedding requests = %d, want 0", got)
	}
}

func TestRunSearchFiltersSnapshotByRepeatedPathAndLanguage(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	store, err := localstore.NewStore(storeDirectory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	chunks := []source.Chunk{
		{ID: "python-src", Text: "shared term", Language: source.LanguagePython, Reference: source.Reference{Path: "src/app.py", StartLine: 1, EndLine: 1}},
		{ID: "typescript-src", Text: "shared term", Language: source.LanguageTypeScript, Reference: source.Reference{Path: "src/app.ts", StartLine: 1, EndLine: 1}},
		{ID: "python-source", Text: "shared term", Language: source.LanguagePython, Reference: source.Reference{Path: "source/app.py", StartLine: 1, EndLine: 1}},
	}
	if err := store.Replace(context.Background(), canonicalPath(t, repository), mustCLICorpus(t, chunks)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{
		"search", "--store", storeDirectory,
		"--path-prefix", "src", "--path-prefix", "tests",
		"--language", "python", "--language", "python",
		repository, "shared", "term",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(search filtered) code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "0.600 src/app.py:1-1\nshared term\n"; got != want {
		t.Fatalf("run(search filtered) stdout = %q, want %q", got, want)
	}
}

func TestRunSearchRejectsInvalidFilterBeforeStoreOrProvider(t *testing.T) {
	clearEmbeddingEnvironment(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)
	repository := t.TempDir()
	for _, test := range []struct {
		name  string
		flag  string
		value string
	}{
		{name: "unsafe path", flag: "--path-prefix", value: "../UNSAFE_PATH_CANARY"},
		{name: "unsupported language", flag: "--language", value: "LANGUAGE_CANARY"},
	} {
		t.Run(test.name, func(t *testing.T) {
			missingStore := filepath.Join(t.TempDir(), "missing-store")
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := run([]string{
				"search", "--store", missingStore, "--index-backend", "auto",
				"--semantic", "preferred", "--embedding-base-url", server.URL + "/v1",
				"--embedding-model", "fixture-model", test.flag, test.value,
				repository, "QUERY_CANARY",
			}, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("run(search invalid filter) code = %d, want 2; stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "invalid search filter") {
				t.Fatalf("run(search invalid filter) stderr = %q, want fixed filter category", stderr.String())
			}
			for _, canary := range []string{"QUERY_CANARY", "UNSAFE_PATH_CANARY", "LANGUAGE_CANARY"} {
				if strings.Contains(stderr.String(), canary) {
					t.Fatalf("run(search invalid filter) stderr exposes %q: %q", canary, stderr.String())
				}
			}
			if _, err := os.Stat(missingStore); !os.IsNotExist(err) {
				t.Fatalf("invalid filter store stat error = %v, want not exist", err)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("invalid filter embedding requests = %d, want 0", got)
	}
}

func TestRunSearchRejectsLegacySnapshotWithoutPrintingCanary(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	store, err := localstore.NewStore(storeDirectory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	repositoryID := canonicalPath(t, repository)
	if err := store.Replace(context.Background(), repositoryID, mustCLICorpus(t, nil)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	entries, err := os.ReadDir(storeDirectory)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("snapshot entries = %d, want 1", len(entries))
	}
	legacy := `{"version":1,"repository_id":"legacy","chunks":[{"id":"legacy","text":"LEGACY_SEARCH_CANARY","language":"python","reference":{"Path":"legacy-secret.py","StartLine":1,"EndLine":1}}]}`
	if err := os.WriteFile(filepath.Join(storeDirectory, entries[0].Name()), []byte(legacy), 0o600); err != nil {
		t.Fatalf("WriteFile(legacy) error = %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"search", "--store", storeDirectory, repository, "canary"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(search legacy) code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "requires reindex") {
		t.Errorf("run(search legacy) stderr = %q, want reindex requirement", stderr.String())
	}
	for _, output := range []string{stdout.String(), stderr.String()} {
		if strings.Contains(output, "LEGACY_SEARCH_CANARY") || strings.Contains(output, "legacy-secret.py") {
			t.Errorf("run(search legacy) output exposes legacy canary: %q", output)
		}
	}
}

func TestRunSearchReportsNoResults(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	store, err := localstore.NewStore(storeDirectory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if err := store.Replace(context.Background(), canonicalPath(t, repository), mustCLICorpus(t, nil)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"search", "--store", storeDirectory, repository, "missing"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(search no results) code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "nenhum resultado\n"; got != want {
		t.Fatalf("run(search no results) stdout = %q, want %q", got, want)
	}
}

func TestRunSearchRejectsInvalidUsage(t *testing.T) {
	clearEmbeddingEnvironment(t)
	tests := [][]string{
		{"search"},
		{"search", "repository-only"},
		{"search", "repository", "   "},
		{"search", "--limit", "0", "repository", "term"},
	}
	for _, args := range tests {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("run(%v) code = %d, want 2", args, code)
		}
		if !strings.Contains(stderr.String(), "uso: gocontext search") {
			t.Errorf("run(%v) stderr = %q, want search usage", args, stderr.String())
		}
	}
}

func TestRunSearchReportsMissingSnapshot(t *testing.T) {
	clearEmbeddingEnvironment(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run([]string{"search", "--store", t.TempDir(), t.TempDir(), "term"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("run(search missing snapshot) code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "consultar repositório") {
		t.Fatalf("run(search missing snapshot) stderr = %q, want operation context", stderr.String())
	}
}

func TestRunSearchEscapesTerminalControlCharacters(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	store, err := localstore.NewStore(storeDirectory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	chunks := []source.Chunk{{
		ID:         "unsafe",
		Text:       "term \x1b[31mred",
		Language:   source.LanguagePython,
		SymbolName: "unsafe\x1b[32msymbol",
		Reference:  source.Reference{Path: "unsafe\x1b[33m.py", StartLine: 1, EndLine: 1},
	}}
	if err := store.Replace(context.Background(), canonicalPath(t, repository), mustCLICorpus(t, chunks)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"search", "--store", storeDirectory, repository, "term"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run(search control characters) code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if strings.ContainsRune(stdout.String(), '\x1b') {
		t.Fatalf("run(search control characters) stdout contains raw ESC: %q", stdout.String())
	}
	for _, escaped := range []string{`unsafe\x1b[33m.py`, `unsafe\x1b[32msymbol`, `\x1b[31mred`} {
		if !strings.Contains(stdout.String(), escaped) {
			t.Fatalf("run(search control characters) stdout = %q, want escaped %q", stdout.String(), escaped)
		}
	}
}

func TestRunSearchRejectsInvalidSemanticConfigurationWithoutEcho(t *testing.T) {
	clearEmbeddingEnvironment(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run([]string{"search", "--semantic=bad\x1b[31mMODE_CANARY", t.TempDir(), "query"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("run(search invalid semantic mode) code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid semantic mode") {
		t.Fatalf("run(search invalid semantic mode) stderr = %q, want fixed configuration category", stderr.String())
	}
	if strings.Contains(stderr.String(), "MODE_CANARY") || strings.ContainsRune(stderr.String(), '\x1b') {
		t.Fatalf("run(search invalid semantic mode) stderr exposes raw value: %q", stderr.String())
	}
}

func TestRunSearchRejectsUnavailableBackendsWithoutSnapshotFallback(t *testing.T) {
	clearEmbeddingEnvironment(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"search", "--store", t.TempDir(), "--index-backend", "sqlite", t.TempDir(), "query"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(search missing sqlite) code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "índice SQLite indisponível") {
		t.Fatalf("run(search missing sqlite) stderr = %q, want fixed unavailable category", stderr.String())
	}
}

func TestRunSearchAutoNeverCreatesMissingStore(t *testing.T) {
	clearEmbeddingEnvironment(t)
	missingStore := filepath.Join(t.TempDir(), "missing-store")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{
		"search", "--store", missingStore, "--index-backend", "auto",
		t.TempDir(), "query",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(search auto missing store) code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if _, err := os.Stat(missingStore); !os.IsNotExist(err) {
		t.Fatalf("Stat(missing store after search) error = %v, want no created state", err)
	}
}

func TestRunSearchMissingSQLiteBackendModeMatrix(t *testing.T) {
	clearEmbeddingEnvironment(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	store, err := localstore.NewStore(storeDirectory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	chunk := source.Chunk{
		ID: "snapshot", Text: "matrix token", Language: source.LanguagePython,
		Reference: source.Reference{Path: "matrix.py", StartLine: 1, EndLine: 1},
	}
	if err := store.Replace(context.Background(), canonicalPath(t, repository), mustCLICorpus(t, []source.Chunk{chunk})); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	tests := []struct {
		name        string
		backend     string
		mode        string
		wantCode    int
		wantHit     bool
		wantWarning bool
	}{
		{name: "auto off", backend: "auto", mode: "off", wantCode: 0, wantHit: true},
		{name: "auto preferred", backend: "auto", mode: "preferred", wantCode: 0, wantHit: true, wantWarning: true},
		{name: "auto required", backend: "auto", mode: "required", wantCode: 1},
		{name: "sqlite off", backend: "sqlite", mode: "off", wantCode: 1},
		{name: "sqlite preferred", backend: "sqlite", mode: "preferred", wantCode: 0, wantHit: true, wantWarning: true},
		{name: "sqlite required", backend: "sqlite", mode: "required", wantCode: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"search", "--store", storeDirectory, "--index-backend", test.backend}
			if test.mode != "off" {
				args = append(args,
					"--semantic", test.mode,
					"--embedding-base-url", server.URL+"/v1",
					"--embedding-model", "fixture-model",
				)
			}
			args = append(args, repository, "matrix", "token")
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if code != test.wantCode {
				t.Fatalf("run(search) code = %d, want %d; stderr = %q", code, test.wantCode, stderr.String())
			}
			if got := strings.Contains(stdout.String(), "matrix.py:1-1"); got != test.wantHit {
				t.Fatalf("run(search) hit = %t, want %t; stdout = %q", got, test.wantHit, stdout.String())
			}
			if got := stderr.String() == semanticDegradedWarning; got != test.wantWarning {
				t.Fatalf("run(search) warning-only = %t, want %t; stderr = %q", got, test.wantWarning, stderr.String())
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("missing SQLite matrix embedding requests = %d, want 0", got)
	}
	if _, err := os.Stat(filepath.Join(storeDirectory, "index-v2.sqlite3")); !os.IsNotExist(err) {
		t.Fatalf("missing SQLite matrix database stat error = %v, want not exist", err)
	}
}

func TestRunSearchAutoDoesNotMaskCorruptSQLiteWithSnapshot(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	store, err := localstore.NewStore(storeDirectory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	chunk := source.Chunk{
		ID: "snapshot-canary", Text: "snapshot canary", Language: source.LanguagePython,
		Reference: source.Reference{Path: "snapshot-canary.py", StartLine: 1, EndLine: 1},
	}
	if err := store.Replace(context.Background(), canonicalPath(t, repository), mustCLICorpus(t, []source.Chunk{chunk})); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(storeDirectory, "index-v2.sqlite3"), []byte("CORRUPT_DB_CANARY"), 0o600); err != nil {
		t.Fatalf("WriteFile(corrupt SQLite) error = %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"search", "--store", storeDirectory, "--index-backend", "auto", repository, "snapshot", "canary"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(search corrupt auto) code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("run(search corrupt auto) stdout = %q, want no snapshot fallback", stdout.String())
	}
	if !strings.Contains(stderr.String(), "índice SQLite inválido") || strings.Contains(stderr.String(), "CORRUPT_DB_CANARY") {
		t.Fatalf("run(search corrupt auto) stderr = %q, want fixed invalid category", stderr.String())
	}
}

func TestRunSearchAutoOffUsesPersistedSQLiteGeneration(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "src/value.py", "def durable_value():\n    return 7\n")
	var indexOut bytes.Buffer
	var indexErr bytes.Buffer
	if code := run([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", repository}, &indexOut, &indexErr); code != 0 {
		t.Fatalf("run(index sqlite off) code = %d; stderr = %q", code, indexErr.String())
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"search", "--store", storeDirectory, "--index-backend", "auto", repository, "durable", "value"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(search auto off) code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "src/value.py:1-2") || !strings.Contains(stdout.String(), "durable_value") {
		t.Fatalf("run(search auto off) stdout = %q, want persisted canonical hit", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("run(search auto off) stderr = %q, want empty", stderr.String())
	}
}

func TestRunSearchExactSQLiteScaleWarningBoundaryAndDefaultSnapshotSilence(t *testing.T) {
	clearEmbeddingEnvironment(t)
	storeDirectory := t.TempDir()
	store, err := indexsqlite.NewStore(storeDirectory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	for _, count := range []int{20_000, 20_001} {
		repository := t.TempDir()
		repositoryID := canonicalPath(t, repository)
		chunks := makeScaleWarningChunks(count)
		corpus := mustCLICorpus(t, chunks)
		generationID := fmt.Sprintf("scale-generation-%d", count)
		if err := store.Replace(context.Background(), indexdomain.Generation{
			RepositoryID: repositoryID, ID: generationID,
			CorpusRevision: corpus.Revision, ScanPolicyVersion: corpus.PolicyVersion,
			Chunks: corpus.Chunks, Metric: indexdomain.VectorMetricCosine,
		}); err != nil {
			t.Fatalf("Replace(%d chunks) error = %v", count, err)
		}

		var stdout bytes.Buffer
		var stderr bytes.Buffer
		code := run([]string{
			"search", "--store", storeDirectory, "--index-backend", "sqlite",
			repository, "scale", "needle",
		}, &stdout, &stderr)
		if code != 0 || !strings.Contains(stdout.String(), "scale_needle") {
			t.Fatalf("run(search %d chunks) code = %d stdout = %q stderr = %q", count, code, stdout.String(), stderr.String())
		}
		wantWarnings := 0
		if count > 20_000 {
			wantWarnings = 1
		}
		if got := strings.Count(stderr.String(), exactSearchScaleWarning); got != wantWarnings {
			t.Fatalf("run(search %d chunks) warning count = %d, want %d; stderr = %q", count, got, wantWarnings, stderr.String())
		}
	}

	repository := t.TempDir()
	repositoryID := canonicalPath(t, repository)
	snapshotStore, err := localstore.NewStore(storeDirectory)
	if err != nil {
		t.Fatalf("NewStore(snapshot) error = %v", err)
	}
	largeSnapshot := makeScaleWarningChunks(20_001)
	if err := snapshotStore.Replace(context.Background(), repositoryID, mustCLICorpus(t, largeSnapshot)); err != nil {
		t.Fatalf("Replace(snapshot) error = %v", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"search", "--store", storeDirectory, repository, "scale", "needle"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "scale_needle") || stderr.Len() != 0 {
		t.Fatalf("run(default snapshot 20001) code = %d stdout = %q stderr = %q, want silent snapshot", code, stdout.String(), stderr.String())
	}
}

func makeScaleWarningChunks(count int) []source.Chunk {
	chunks := make([]source.Chunk, count)
	for position := range chunks {
		text := "def filler_value():\n    return 1"
		if position == 0 {
			text = "def scale_needle():\n    return 1"
		}
		chunks[position] = source.Chunk{
			ID: fmt.Sprintf("scale-%05d", position), Text: text,
			Language: source.LanguagePython,
			Reference: source.Reference{
				Path: fmt.Sprintf("src/scale-%05d.py", position), StartLine: 1, EndLine: 2,
			},
		}
	}
	return chunks
}

func TestRunSearchExplicitSnapshotRequiresCurrentRollbackMarkerWhenSQLiteActive(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository, storeDirectory, marker := prepareCLIRollbackFixture(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{
		"search", "--store", storeDirectory, "--index-backend", "snapshot",
		repository, "rollback", "value",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(search current rollback) code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "rollback_value") || stderr.Len() != 0 {
		t.Fatalf("run(search current rollback) stdout = %q stderr = %q, want current snapshot", stdout.String(), stderr.String())
	}

	markerPath := rollbackMarkerPath(storeDirectory, canonicalPath(t, repository))
	if err := os.Remove(markerPath); err != nil {
		t.Fatalf("Remove(rollback marker) error = %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"search", "--store", storeDirectory, repository, "rollback", "value"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "rollback_value") || stderr.Len() != 0 {
		t.Fatalf("run(search implicit snapshot without marker) code = %d stdout = %q stderr = %q, want compatibility snapshot", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"search", "--store", storeDirectory, "--index-backend", "snapshot",
		repository, "rollback", "value",
	}, &stdout, &stderr)
	assertRollbackReindexFailure(t, code, stdout.String(), stderr.String(), repository, marker.ActiveGeneration)
}

func TestSnapshotRollbackFailureCarriesReindexCategoryWithoutChangingText(t *testing.T) {
	if !errors.Is(errSnapshotRollbackReindex, indexdomain.ErrReindexRequired) {
		t.Fatal("snapshot rollback error does not unwrap to ErrReindexRequired")
	}
	if got, want := errSnapshotRollbackReindex.Error(), "rollback de snapshot exige reindexação"; got != want {
		t.Fatalf("snapshot rollback error = %q, want fixed %q", got, want)
	}
}

func TestRunSearchExplicitSnapshotRejectsInvalidRollbackMarkerMatrix(t *testing.T) {
	clearEmbeddingEnvironment(t)
	tests := []struct {
		name   string
		mutate func(*testing.T, string, rollbackMarker)
	}{
		{
			name: "malformed",
			mutate: func(t *testing.T, path string, _ rollbackMarker) {
				writeRollbackTestPayload(t, path, []byte(`{"version":`))
			},
		},
		{
			name: "extra field",
			mutate: func(t *testing.T, path string, marker rollbackMarker) {
				payload, err := json.Marshal(struct {
					rollbackMarker
					Canary string `json:"EXTRA_MARKER_CANARY"`
				}{rollbackMarker: marker, Canary: "EXTRA_MARKER_CANARY"})
				if err != nil {
					t.Fatalf("Marshal(extra marker) error = %v", err)
				}
				writeRollbackTestPayload(t, path, payload)
			},
		},
		{
			name: "duplicate field",
			mutate: func(t *testing.T, path string, marker rollbackMarker) {
				payload, err := json.Marshal(marker)
				if err != nil {
					t.Fatalf("Marshal(marker) error = %v", err)
				}
				payload = append(payload[:len(payload)-1], []byte(`,"version":1}`)...)
				writeRollbackTestPayload(t, path, payload)
			},
		},
		{
			name: "oversize",
			mutate: func(t *testing.T, path string, _ rollbackMarker) {
				writeRollbackTestPayload(t, path, bytes.Repeat([]byte("OVERSIZE_MARKER_CANARY"), 256))
			},
		},
		{
			name: "permissive",
			mutate: func(t *testing.T, path string, _ rollbackMarker) {
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatalf("Chmod(marker) error = %v", err)
				}
			},
		},
		{
			name: "inaccessible",
			mutate: func(t *testing.T, path string, _ rollbackMarker) {
				if err := os.Chmod(path, 0o000); err != nil {
					t.Fatalf("Chmod(marker inaccessible) error = %v", err)
				}
			},
		},
		{
			name: "non regular",
			mutate: func(t *testing.T, path string, _ rollbackMarker) {
				if err := os.Remove(path); err != nil {
					t.Fatalf("Remove(marker) error = %v", err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("Mkdir(marker path) error = %v", err)
				}
			},
		},
		{
			name: "symlink",
			mutate: func(t *testing.T, path string, marker rollbackMarker) {
				if err := os.Remove(path); err != nil {
					t.Fatalf("Remove(marker) error = %v", err)
				}
				target := filepath.Join(filepath.Dir(path), "SYMLINK_MARKER_CANARY.json")
				payload, err := json.Marshal(marker)
				if err != nil {
					t.Fatalf("Marshal(marker) error = %v", err)
				}
				if err := os.WriteFile(target, payload, 0o600); err != nil {
					t.Fatalf("WriteFile(symlink target) error = %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("Symlink() unavailable: %v", err)
					}
					t.Fatalf("Symlink(marker) error = %v", err)
				}
			},
		},
		{
			name: "repository mismatch",
			mutate: func(t *testing.T, path string, marker rollbackMarker) {
				marker.RepositoryHash = strings.Repeat("a", 64)
				writeRollbackTestMarker(t, path, marker)
			},
		},
		{
			name: "policy mismatch",
			mutate: func(t *testing.T, path string, marker rollbackMarker) {
				marker.ScanPolicy = "POLICY_MARKER_CANARY"
				writeRollbackTestMarker(t, path, marker)
			},
		},
		{
			name: "revision mismatch",
			mutate: func(t *testing.T, path string, marker rollbackMarker) {
				marker.CorpusRevision = strings.Repeat("b", 64)
				writeRollbackTestMarker(t, path, marker)
			},
		},
		{
			name: "generation mismatch",
			mutate: func(t *testing.T, path string, marker rollbackMarker) {
				marker.ActiveGeneration = strings.Repeat("c", 64)
				writeRollbackTestMarker(t, path, marker)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository, storeDirectory, marker := prepareCLIRollbackFixture(t)
			markerPath := rollbackMarkerPath(storeDirectory, canonicalPath(t, repository))
			test.mutate(t, markerPath, marker)

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := run([]string{
				"search", "--store", storeDirectory, "--index-backend", "snapshot",
				repository, "rollback", "value",
			}, &stdout, &stderr)
			assertRollbackReindexFailure(t, code, stdout.String(), stderr.String(),
				repository, marker.ActiveGeneration, "MARKER_CANARY")
		})
	}
}

func TestRunSearchExplicitSnapshotRejectsStaleSnapshotAndSwappedActiveGeneration(t *testing.T) {
	clearEmbeddingEnvironment(t)
	t.Run("stale snapshot", func(t *testing.T) {
		repository, storeDirectory, marker := prepareCLIRollbackFixture(t)
		repositoryID := canonicalPath(t, repository)
		staleChunk := source.Chunk{
			ID: "stale-snapshot", Text: "def stale_snapshot_value():\n    return 2",
			Language:  source.LanguagePython,
			Reference: source.Reference{Path: "stale.py", StartLine: 1, EndLine: 2},
		}
		snapshotStore, err := localstore.NewStore(storeDirectory)
		if err != nil {
			t.Fatalf("NewStore(snapshot) error = %v", err)
		}
		if err := snapshotStore.Replace(context.Background(), repositoryID, mustCLICorpus(t, []source.Chunk{staleChunk})); err != nil {
			t.Fatalf("Replace(stale snapshot) error = %v", err)
		}

		var stdout bytes.Buffer
		var stderr bytes.Buffer
		code := run([]string{
			"search", "--store", storeDirectory, "--index-backend", "snapshot",
			repository, "stale", "snapshot",
		}, &stdout, &stderr)
		assertRollbackReindexFailure(t, code, stdout.String(), stderr.String(),
			repository, marker.ActiveGeneration, "stale_snapshot_value")
	})

	t.Run("swapped active generation", func(t *testing.T) {
		repository, storeDirectory, marker := prepareCLIRollbackFixture(t)
		repositoryID := canonicalPath(t, repository)
		newChunk := source.Chunk{
			ID: "swapped-generation", Text: "def swapped_generation_value():\n    return 3",
			Language:  source.LanguagePython,
			Reference: source.Reference{Path: "swapped.py", StartLine: 1, EndLine: 2},
		}
		corpus := mustCLICorpus(t, []source.Chunk{newChunk})
		newGenerationID := strings.Repeat("d", 64)
		var hookCalls atomic.Int64
		hits, err := searchExplicitSnapshotRollbackWithHooks(
			context.Background(),
			storeDirectory,
			searchdomain.Query{RepositoryID: repositoryID, Text: "rollback value", Limit: defaultSearchLimit},
			snapshotRollbackHooks{afterBind: func(context.Context, *indexsqlite.BoundReader) error {
				hookCalls.Add(1)
				store, err := indexsqlite.NewStore(storeDirectory)
				if err != nil {
					return err
				}
				defer store.Close()
				err = store.Replace(context.Background(), indexdomain.Generation{
					RepositoryID: repositoryID, ID: newGenerationID, BaseGeneration: marker.ActiveGeneration,
					CorpusRevision: corpus.Revision, ScanPolicyVersion: corpus.PolicyVersion,
					Chunks: corpus.Chunks, Metric: indexdomain.VectorMetricCosine,
				})
				var committed *indexdomain.CommittedCleanupError
				if err != nil && !errors.As(err, &committed) {
					return err
				}
				return nil
			}},
		)
		if err != nil {
			t.Fatalf("searchExplicitSnapshotRollbackWithHooks(swapped active) error = %v", err)
		}
		if hookCalls.Load() != 1 || len(hits) == 0 || !strings.Contains(hits[0].Chunk.Text, "rollback_value") {
			t.Fatalf("pinned rollback hits = %#v, hook calls = %d; want old pinned snapshot", hits, hookCalls.Load())
		}
		store, err := indexsqlite.OpenExisting(storeDirectory)
		if err != nil {
			t.Fatalf("OpenExisting(after swap) error = %v", err)
		}
		active, activeErr := store.ActiveGeneration(context.Background(), repositoryID)
		closeErr := store.Close()
		if activeErr != nil || closeErr != nil || active != newGenerationID {
			t.Fatalf("active generation after pinned rollback = %q, active error %v, close error %v; want %q", active, activeErr, closeErr, newGenerationID)
		}
	})
}

func TestSearchExplicitSnapshotRollbackPreservesContextCategoriesAfterBind(t *testing.T) {
	t.Run("canceled", func(t *testing.T) {
		repository, storeDirectory, _ := prepareCLIRollbackFixture(t)
		repositoryID := canonicalPath(t, repository)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := searchExplicitSnapshotRollbackWithHooks(
			ctx,
			storeDirectory,
			searchdomain.Query{RepositoryID: repositoryID, Text: "rollback value", Limit: defaultSearchLimit},
			snapshotRollbackHooks{afterBind: func(context.Context, *indexsqlite.BoundReader) error {
				cancel()
				return nil
			}},
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("searchExplicitSnapshotRollbackWithHooks(canceled) error = %v, want context.Canceled", err)
		}
		if errors.Is(err, indexdomain.ErrReindexRequired) {
			t.Fatalf("searchExplicitSnapshotRollbackWithHooks(canceled) error = %v, must not map cancellation to reindex", err)
		}
	})

	t.Run("deadline exceeded", func(t *testing.T) {
		repository, storeDirectory, _ := prepareCLIRollbackFixture(t)
		repositoryID := canonicalPath(t, repository)
		_, err := searchExplicitSnapshotRollbackWithHooks(
			context.Background(),
			storeDirectory,
			searchdomain.Query{RepositoryID: repositoryID, Text: "rollback value", Limit: defaultSearchLimit},
			snapshotRollbackHooks{afterBind: func(context.Context, *indexsqlite.BoundReader) error {
				return context.DeadlineExceeded
			}},
		)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("searchExplicitSnapshotRollbackWithHooks(deadline) error = %v, want context.DeadlineExceeded", err)
		}
		if errors.Is(err, indexdomain.ErrReindexRequired) {
			t.Fatalf("searchExplicitSnapshotRollbackWithHooks(deadline) error = %v, must not map deadline to reindex", err)
		}
	})
}

func TestRunSearchExplicitSnapshotAllowsAbsentSQLiteRepositoryButRejectsCorruptSQLite(t *testing.T) {
	clearEmbeddingEnvironment(t)
	requestedRepository := t.TempDir()
	otherRepository := t.TempDir()
	storeDirectory := t.TempDir()
	writeCLIFile(t, requestedRepository, "requested.py", "def requested_value():\n    return 1\n")
	writeCLIFile(t, otherRepository, "other.py", "def other_value():\n    return 2\n")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"index", "--store", storeDirectory, requestedRepository}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(index requested snapshot) code = %d; stderr = %q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", otherRepository}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(index other SQLite) code = %d; stderr = %q", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code := run([]string{
		"search", "--store", storeDirectory, "--index-backend", "snapshot",
		requestedRepository, "requested", "value",
	}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "requested_value") || stderr.Len() != 0 {
		t.Fatalf("run(search snapshot with other SQLite repository) code = %d stdout = %q stderr = %q", code, stdout.String(), stderr.String())
	}

	corruptStore := t.TempDir()
	corruptSnapshot, err := localstore.NewStore(corruptStore)
	if err != nil {
		t.Fatalf("NewStore(corrupt fixture) error = %v", err)
	}
	chunk := source.Chunk{
		ID: "corrupt-snapshot", Text: "CORRUPT_SNAPSHOT_CANARY",
		Language:  source.LanguagePython,
		Reference: source.Reference{Path: "corrupt.py", StartLine: 1, EndLine: 1},
	}
	if err := corruptSnapshot.Replace(context.Background(), canonicalPath(t, requestedRepository), mustCLICorpus(t, []source.Chunk{chunk})); err != nil {
		t.Fatalf("Replace(corrupt fixture snapshot) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(corruptStore, "index-v2.sqlite3"), []byte("CORRUPT_SQLITE_CANARY"), 0o600); err != nil {
		t.Fatalf("WriteFile(corrupt SQLite) error = %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"search", "--store", corruptStore,
		requestedRepository, "corrupt", "snapshot",
	}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "CORRUPT_SNAPSHOT_CANARY") || stderr.Len() != 0 {
		t.Fatalf("run(implicit snapshot beside corrupt SQLite) code = %d stdout = %q stderr = %q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"search", "--store", corruptStore, "--index-backend", "snapshot",
		requestedRepository, "corrupt", "snapshot",
	}, &stdout, &stderr)
	assertRollbackReindexFailure(t, code, stdout.String(), stderr.String(),
		requestedRepository, "CORRUPT_SQLITE_CANARY", "CORRUPT_SNAPSHOT_CANARY")
}

func TestRunSearchExplicitSnapshotRejectsCorruptPinnedSQLiteCorpus(t *testing.T) {
	clearEmbeddingEnvironment(t)
	mutations := []struct {
		name  string
		query string
	}{
		{name: "deleted canonical chunk", query: `DELETE FROM chunks`},
		{name: "modified canonical chunk", query: `UPDATE chunks SET text = 'PRIVATE_MODIFIED_CHUNK_CANARY'`},
		{name: "deleted vector", query: `DELETE FROM vectors`},
		{name: "modified vector", query: `UPDATE vectors SET values_blob = X'010203'`},
		{name: "modified generation manifest", query: `UPDATE generations SET corpus_revision = 'PRIVATE_MODIFIED_MANIFEST_CANARY'`},
		{name: "dangling active generation", query: `UPDATE repositories SET active_generation = 'PRIVATE_DANGLING_GENERATION_CANARY'`},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			repository, storeDirectory, generation := prepareCLIRollbackVectorFixture(t)
			database, err := sql.Open("sqlite", filepath.Join(storeDirectory, "index-v2.sqlite3"))
			if err != nil {
				t.Fatalf("sql.Open() error = %v", err)
			}
			if _, err := database.Exec(test.query); err != nil {
				_ = database.Close()
				t.Fatalf("mutate SQLite corpus error = %v", err)
			}
			if err := database.Close(); err != nil {
				t.Fatalf("close mutation database error = %v", err)
			}

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := run([]string{
				"search", "--store", storeDirectory, "--index-backend", "snapshot",
				repository, "rollback", "value",
			}, &stdout, &stderr)
			assertRollbackReindexFailure(t, code, stdout.String(), stderr.String(),
				repository, generation.ID, "PRIVATE_MODIFIED_CHUNK_CANARY",
				"PRIVATE_MODIFIED_MANIFEST_CANARY", "PRIVATE_DANGLING_GENERATION_CANARY")
		})
	}
}

func prepareCLIRollbackVectorFixture(t *testing.T) (string, string, indexdomain.Generation) {
	t.Helper()
	repository := t.TempDir()
	repositoryID := canonicalPath(t, repository)
	storeDirectory := t.TempDir()
	chunk := source.Chunk{
		ID: "rollback-vector", Text: "def rollback_value():\n    return 1",
		Language:  source.LanguagePython,
		Reference: source.Reference{Path: "rollback.py", StartLine: 1, EndLine: 2},
	}
	corpus := mustCLICorpus(t, []source.Chunk{chunk})
	generation := indexdomain.Generation{
		RepositoryID: repositoryID, ID: strings.Repeat("a", 64),
		CorpusRevision: corpus.Revision, ScanPolicyVersion: corpus.PolicyVersion,
		Chunks:     corpus.Chunks,
		Profile:    &embedding.Profile{Fingerprint: "rollback-vector-profile", Model: "rollback-vector-model"},
		Dimensions: 2, Metric: indexdomain.VectorMetricCosine,
		Vectors: []indexdomain.VectorRecord{{ChunkID: chunk.ID, Values: embedding.Vector{1, 0}}},
	}
	store, err := indexsqlite.NewStore(storeDirectory)
	if err != nil {
		t.Fatalf("NewStore(SQLite) error = %v", err)
	}
	if err := store.Replace(context.Background(), generation); err != nil {
		_ = store.Close()
		t.Fatalf("Replace(SQLite) error = %v", err)
	}
	if err := writeRollbackCompanion(
		context.Background(),
		storeDirectory,
		repositoryIngest{repositoryID: repositoryID, corpus: corpus},
		generation.ID,
		store,
	); err != nil {
		_ = store.Close()
		t.Fatalf("writeRollbackCompanion() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(SQLite) error = %v", err)
	}
	return repository, storeDirectory, generation
}

func prepareCLIRollbackFixture(t *testing.T) (string, string, rollbackMarker) {
	t.Helper()
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "rollback.py", "def rollback_value():\n    return 1\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", repository}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(index rollback fixture) code = %d; stderr = %q", code, stderr.String())
	}
	payload, err := os.ReadFile(rollbackMarkerPath(storeDirectory, canonicalPath(t, repository)))
	if err != nil {
		t.Fatalf("ReadFile(rollback marker) error = %v", err)
	}
	var marker rollbackMarker
	if err := json.Unmarshal(payload, &marker); err != nil {
		t.Fatalf("Unmarshal(rollback marker) error = %v", err)
	}
	return repository, storeDirectory, marker
}

func writeRollbackTestMarker(t *testing.T, path string, marker rollbackMarker) {
	t.Helper()
	payload, err := json.Marshal(marker)
	if err != nil {
		t.Fatalf("Marshal(marker) error = %v", err)
	}
	writeRollbackTestPayload(t, path, payload)
}

func writeRollbackTestPayload(t *testing.T, path string, payload []byte) {
	t.Helper()
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("WriteFile(marker) error = %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("Chmod(marker) error = %v", err)
	}
}

func assertRollbackReindexFailure(t *testing.T, code int, stdout, stderr string, canaries ...string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("run(search invalid rollback) code = %d, want 1; stderr = %q", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("run(search invalid rollback) stdout = %q, want empty", stdout)
	}
	const want = "consultar repositório: rollback de snapshot exige reindexação\n"
	if stderr != want {
		t.Fatalf("run(search invalid rollback) stderr = %q, want fixed %q", stderr, want)
	}
	for _, canary := range canaries {
		if canary != "" && strings.Contains(stderr, canary) {
			t.Fatalf("run(search invalid rollback) stderr exposes %q: %q", canary, stderr)
		}
	}
}

func TestRunSearchSemanticSnapshotRequiresOptInBackend(t *testing.T) {
	clearEmbeddingEnvironment(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run([]string{
		"search",
		"--semantic", "required",
		"--embedding-base-url", "https://api.example.test/v1",
		"--embedding-model", "example-model",
		t.TempDir(),
		"query",
	}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("run(search semantic snapshot) code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--index-backend sqlite or auto") {
		t.Fatalf("run(search semantic snapshot) stderr = %q, want opt-in backend requirement", stderr.String())
	}
}

func TestRunSearchRejectsAPIKeyFlagWithoutEcho(t *testing.T) {
	clearEmbeddingEnvironment(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run([]string{"search", "--embedding-api-key=KEY_CLI_CANARY\x1b[31m", t.TempDir(), "query"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("run(search API key flag) code = %d, want 2", code)
	}
	if strings.Contains(stderr.String(), "KEY_CLI_CANARY") || strings.ContainsRune(stderr.String(), '\x1b') {
		t.Fatalf("run(search API key flag) stderr exposes key: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "API_KEY") {
		t.Fatalf("run(search API key flag) stderr exposes environment mechanism: %q", stderr.String())
	}
}

func mustCLICorpus(t *testing.T, chunks []source.Chunk) source.Corpus {
	t.Helper()
	corpus, err := source.NewCorpus(ingest.ScanPolicyVersion, chunks)
	if err != nil {
		t.Fatalf("NewCorpus() error = %v", err)
	}
	return corpus
}
