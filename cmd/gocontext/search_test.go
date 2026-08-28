package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
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
