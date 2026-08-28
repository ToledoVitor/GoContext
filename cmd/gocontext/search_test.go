package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestRunSearchPrintsRankedCitationAndText(t *testing.T) {
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
	if err := store.Replace(context.Background(), canonicalPath(t, repository), chunks); err != nil {
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
	for _, fragment := range []string{
		"0.950 src/user.py:3-4 LoadUser",
		"def load_user():\n    return user",
	} {
		if !strings.Contains(stdout.String(), fragment) {
			t.Errorf("run(search) stdout = %q, want fragment %q", stdout.String(), fragment)
		}
	}
	if strings.Contains(stdout.String(), "partial") || strings.Contains(stdout.String(), "src/load.py") {
		t.Errorf("run(search) stdout = %q, want limit 1", stdout.String())
	}
}

func TestRunSearchReportsNoResults(t *testing.T) {
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	store, err := localstore.NewStore(storeDirectory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	if err := store.Replace(context.Background(), canonicalPath(t, repository), nil); err != nil {
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
	tests := [][]string{
		{"search"},
		{"search", "repository-only"},
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
	if err := store.Replace(context.Background(), canonicalPath(t, repository), chunks); err != nil {
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
