package symbolchunker_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest/symbolchunker"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestChunkerCreatesOneChunkPerSymbolBody(t *testing.T) {
	file := source.File{
		Reference: source.Reference{Path: "src/app.py", StartLine: 1, EndLine: 9},
		Language:  source.LanguagePython,
		Content: []byte("" +
			"from pathlib import Path\n" +
			"\n" +
			"def load():\n" +
			"    return Path('.')\n" +
			"\n" +
			"class Repository:\n" +
			"    def save(self):\n" +
			"        return True\n" +
			"\n"),
	}
	symbols := []source.Symbol{
		{Name: "load", Kind: "function", Signature: "def load():", Reference: source.Reference{Path: "src/app.py", StartLine: 3, EndLine: 3}},
		{Name: "Repository", Kind: "class", Signature: "class Repository:", Reference: source.Reference{Path: "src/app.py", StartLine: 6, EndLine: 6}},
	}

	chunks, err := symbolchunker.NewChunker().Chunk(context.Background(), file, symbols)
	if err != nil {
		t.Fatalf("Chunk() error = %v", err)
	}

	want := []source.Chunk{
		{
			Text:       "def load():\n    return Path('.')",
			Language:   source.LanguagePython,
			SymbolName: "load",
			Reference:  source.Reference{Path: "src/app.py", StartLine: 3, EndLine: 4},
		},
		{
			Text:       "class Repository:\n    def save(self):\n        return True",
			Language:   source.LanguagePython,
			SymbolName: "Repository",
			Reference:  source.Reference{Path: "src/app.py", StartLine: 6, EndLine: 8},
		},
	}
	assertChunks(t, chunks, want)
}

func TestChunkerFallsBackToWholeNonEmptyFile(t *testing.T) {
	file := source.File{
		Reference: source.Reference{Path: "src/constants.ts", StartLine: 1, EndLine: 2},
		Language:  source.LanguageTypeScript,
		Content:   []byte("export const answer = 42\n\n"),
	}

	chunks, err := symbolchunker.NewChunker().Chunk(context.Background(), file, nil)
	if err != nil {
		t.Fatalf("Chunk() error = %v", err)
	}

	want := []source.Chunk{{
		Text:      "export const answer = 42",
		Language:  source.LanguageTypeScript,
		Reference: source.Reference{Path: "src/constants.ts", StartLine: 1, EndLine: 1},
	}}
	assertChunks(t, chunks, want)
}

func TestChunkerProducesStableContentSensitiveIDs(t *testing.T) {
	chunker := symbolchunker.NewChunker()
	file := source.File{
		Reference: source.Reference{Path: "app.py", StartLine: 1, EndLine: 1},
		Language:  source.LanguagePython,
		Content:   []byte("VALUE = 1\n"),
	}

	first, err := chunker.Chunk(context.Background(), file, nil)
	if err != nil {
		t.Fatalf("first Chunk() error = %v", err)
	}
	second, err := chunker.Chunk(context.Background(), file, nil)
	if err != nil {
		t.Fatalf("second Chunk() error = %v", err)
	}
	if first[0].ID == "" || first[0].ID != second[0].ID {
		t.Fatalf("stable IDs = %q and %q, want same non-empty ID", first[0].ID, second[0].ID)
	}

	file.Content = []byte("VALUE = 2\n")
	changed, err := chunker.Chunk(context.Background(), file, nil)
	if err != nil {
		t.Fatalf("changed Chunk() error = %v", err)
	}
	if changed[0].ID == first[0].ID {
		t.Fatalf("content-sensitive IDs both = %q, want different IDs", first[0].ID)
	}
}

func TestChunkerRejectsUnsafeOrUnorderedSymbols(t *testing.T) {
	file := source.File{
		Reference: source.Reference{Path: "app.py", StartLine: 1, EndLine: 3},
		Language:  source.LanguagePython,
		Content:   []byte("def first():\n    pass\ndef second():\n"),
	}
	tests := []struct {
		name    string
		file    source.File
		symbols []source.Symbol
	}{
		{
			name: "invalid file reference",
			file: source.File{Reference: source.Reference{Path: "../app.py", StartLine: 1, EndLine: 1}, Language: source.LanguagePython, Content: []byte("x\n")},
		},
		{
			name:    "different symbol path",
			file:    file,
			symbols: []source.Symbol{{Name: "first", Reference: source.Reference{Path: "other.py", StartLine: 1, EndLine: 1}}},
		},
		{
			name:    "symbol outside content",
			file:    file,
			symbols: []source.Symbol{{Name: "missing", Reference: source.Reference{Path: "app.py", StartLine: 4, EndLine: 4}}},
		},
		{
			name: "unordered symbols",
			file: file,
			symbols: []source.Symbol{
				{Name: "second", Reference: source.Reference{Path: "app.py", StartLine: 3, EndLine: 3}},
				{Name: "first", Reference: source.Reference{Path: "app.py", StartLine: 1, EndLine: 1}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := symbolchunker.NewChunker().Chunk(context.Background(), tt.file, tt.symbols)
			if !errors.Is(err, symbolchunker.ErrInvalidInput) {
				t.Fatalf("Chunk() error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestChunkerRespectsCancellationAndSkipsWhitespaceOnlyFiles(t *testing.T) {
	chunker := symbolchunker.NewChunker()
	file := source.File{
		Reference: source.Reference{Path: "app.py", StartLine: 1, EndLine: 1},
		Language:  source.LanguagePython,
		Content:   []byte("  \n"),
	}

	chunks, err := chunker.Chunk(context.Background(), file, nil)
	if err != nil {
		t.Fatalf("Chunk() whitespace error = %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("Chunk() whitespace returned %d chunks, want 0", len(chunks))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = chunker.Chunk(ctx, file, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Chunk() canceled error = %v, want context.Canceled", err)
	}
}

func assertChunks(t *testing.T, got, want []source.Chunk) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Chunk() returned %d chunks, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].ID == "" {
			t.Errorf("chunks[%d].ID is empty", i)
		}
		gotWithoutID := got[i]
		gotWithoutID.ID = ""
		if gotWithoutID != want[i] {
			t.Errorf("chunks[%d] = %#v, want %#v", i, gotWithoutID, want[i])
		}
	}
}
