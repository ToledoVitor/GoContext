package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestRunSearchDefaultPrintsRankedCitationAndText(t *testing.T) {
	clearEmbeddingEnvironment(t)
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
		ID:        "unsafe",
		Text:      "term \x1b[31mred",
		Language:  source.LanguagePython,
		Reference: source.Reference{Path: "unsafe.py", StartLine: 1, EndLine: 1},
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
	if !strings.Contains(stdout.String(), `\x1b[31mred`) {
		t.Fatalf("run(search control characters) stdout = %q, want escaped control", stdout.String())
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
	for _, backend := range []string{"sqlite", "auto"} {
		t.Run(backend, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			code := run([]string{"search", "--index-backend", backend, t.TempDir(), "query"}, &stdout, &stderr)

			if code != 1 {
				t.Fatalf("run(search %s not wired) code = %d, want 1; stderr = %q", backend, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "not wired yet") {
				t.Fatalf("run(search %s not wired) stderr = %q, want fixed operational category", backend, stderr.String())
			}
		})
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
