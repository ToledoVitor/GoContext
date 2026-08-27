package lineparser_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest/lineparser"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestParserFindsTopLevelPythonDeclarations(t *testing.T) {
	file := source.File{
		Reference: source.Reference{Path: "src/app.py", StartLine: 1, EndLine: 12},
		Language:  source.LanguagePython,
		Content: []byte("" +
			"from typing import Any\n" +
			"\n" +
			"def load_data(path: str) -> Any:\n" +
			"    def normalize():\n" +
			"        pass\n" +
			"    return normalize()\n" +
			"\n" +
			"async def refresh() -> None:\n" +
			"    pass\n" +
			"class Repository:\n" +
			"    def save(self):\n" +
			"        pass\n"),
	}

	symbols, err := lineparser.NewParser().Parse(context.Background(), file)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	want := []source.Symbol{
		{Name: "load_data", Kind: "function", Signature: "def load_data(path: str) -> Any:", Reference: source.Reference{Path: "src/app.py", StartLine: 3, EndLine: 3}},
		{Name: "refresh", Kind: "function", Signature: "async def refresh() -> None:", Reference: source.Reference{Path: "src/app.py", StartLine: 8, EndLine: 8}},
		{Name: "Repository", Kind: "class", Signature: "class Repository:", Reference: source.Reference{Path: "src/app.py", StartLine: 10, EndLine: 10}},
	}
	assertSymbols(t, symbols, want)
}

func TestParserFindsTopLevelTypeScriptDeclarations(t *testing.T) {
	file := source.File{
		Reference: source.Reference{Path: "src/app.ts", StartLine: 1, EndLine: 13},
		Language:  source.LanguageTypeScript,
		Content: []byte("" +
			"import { Client } from './client'\n" +
			"\n" +
			"export async function loadData(): Promise<void> {\n" +
			"}\n" +
			"\n" +
			"export default class Repository {\n" +
			"  save(): void {}\n" +
			"}\n" +
			"export interface Options {}\n" +
			"export type Result = string\n" +
			"export const enum State { Ready }\n" +
			"const arrow = () => true\n" +
			"  function nested() {}\n"),
	}

	symbols, err := lineparser.NewParser().Parse(context.Background(), file)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	want := []source.Symbol{
		{Name: "loadData", Kind: "function", Signature: "export async function loadData(): Promise<void> {", Reference: source.Reference{Path: "src/app.ts", StartLine: 3, EndLine: 3}},
		{Name: "Repository", Kind: "class", Signature: "export default class Repository {", Reference: source.Reference{Path: "src/app.ts", StartLine: 6, EndLine: 6}},
		{Name: "Options", Kind: "interface", Signature: "export interface Options {}", Reference: source.Reference{Path: "src/app.ts", StartLine: 9, EndLine: 9}},
		{Name: "Result", Kind: "type", Signature: "export type Result = string", Reference: source.Reference{Path: "src/app.ts", StartLine: 10, EndLine: 10}},
		{Name: "State", Kind: "enum", Signature: "export const enum State { Ready }", Reference: source.Reference{Path: "src/app.ts", StartLine: 11, EndLine: 11}},
	}
	assertSymbols(t, symbols, want)
}

func TestParserRejectsUnsupportedLanguageAndInvalidReference(t *testing.T) {
	parser := lineparser.NewParser()

	_, err := parser.Parse(context.Background(), source.File{
		Reference: source.Reference{Path: "README.md", StartLine: 1, EndLine: 1},
		Language:  source.LanguageUnknown,
		Content:   []byte("text\n"),
	})
	if !errors.Is(err, lineparser.ErrUnsupportedLanguage) {
		t.Fatalf("Parse() error = %v, want ErrUnsupportedLanguage", err)
	}

	_, err = parser.Parse(context.Background(), source.File{
		Reference: source.Reference{Path: "../app.py", StartLine: 1, EndLine: 1},
		Language:  source.LanguagePython,
		Content:   []byte("def main():\n"),
	})
	if !errors.Is(err, lineparser.ErrInvalidReference) {
		t.Fatalf("Parse() error = %v, want ErrInvalidReference", err)
	}
}

func TestParserRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := lineparser.NewParser().Parse(ctx, source.File{
		Reference: source.Reference{Path: "app.py", StartLine: 1, EndLine: 1},
		Language:  source.LanguagePython,
		Content:   []byte("def main():\n"),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Parse() error = %v, want context.Canceled", err)
	}
}

func assertSymbols(t *testing.T, got, want []source.Symbol) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Parse() returned %d symbols, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("symbols[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}
