package lineparser_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest/lineparser"
	"github.com/ToledoVitor/GoContext/internal/source"
)

const javaScriptCancellationTestStride = 4 << 10

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

func TestParserFindsConservativeTopLevelJavaScriptDeclarations(t *testing.T) {
	file := source.File{
		Reference: source.Reference{Path: "src/view.jsx", StartLine: 7, EndLine: 26},
		Language:  source.LanguageJavaScript,
		Content: []byte("" +
			"export async function loadData() {}\r\n" +
			"function* iterate() {}\r\n" +
			"export default class Repository {}\r\n" +
			"export const View = (props) => <main />\r\n" +
			"const helper = function (value) { return value }\r\n" +
			"let refresh = async value => value\r\n" +
			"var sequence = async function* named() {}\r\n" +
			"function render() {}\r\n" +
			"const answer = 42\r\n" +
			"export default function () {}\r\n" +
			"export default class {}\r\n" +
			"  function nested() {}\r\n" +
			"\tconst nestedArrow = () => true\r\n" +
			"const malformed = (value) = value\r\n" +
			"export default const invalid = () => true\r\n" +
			"const constructed = class Named {}\r\n" +
			"// function commentedOut() {}\r\n" +
			"function default() {}\r\n" +
			"class return {}\r\n" +
			"const invalidKeyword = function => true\r\n"),
	}

	symbols, err := lineparser.NewParser().Parse(context.Background(), file)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	want := []source.Symbol{
		{Name: "loadData", Kind: "function", Signature: "export async function loadData() {}", Reference: source.Reference{Path: "src/view.jsx", StartLine: 7, EndLine: 7}},
		{Name: "iterate", Kind: "function", Signature: "function* iterate() {}", Reference: source.Reference{Path: "src/view.jsx", StartLine: 8, EndLine: 8}},
		{Name: "Repository", Kind: "class", Signature: "export default class Repository {}", Reference: source.Reference{Path: "src/view.jsx", StartLine: 9, EndLine: 9}},
		{Name: "View", Kind: "function", Signature: "export const View = (props) => <main />", Reference: source.Reference{Path: "src/view.jsx", StartLine: 10, EndLine: 10}},
		{Name: "helper", Kind: "function", Signature: "const helper = function (value) { return value }", Reference: source.Reference{Path: "src/view.jsx", StartLine: 11, EndLine: 11}},
		{Name: "refresh", Kind: "function", Signature: "let refresh = async value => value", Reference: source.Reference{Path: "src/view.jsx", StartLine: 12, EndLine: 12}},
		{Name: "sequence", Kind: "function", Signature: "var sequence = async function* named() {}", Reference: source.Reference{Path: "src/view.jsx", StartLine: 13, EndLine: 13}},
		{Name: "render", Kind: "function", Signature: "function render() {}", Reference: source.Reference{Path: "src/view.jsx", StartLine: 14, EndLine: 14}},
	}
	assertSymbols(t, symbols, want)
}

func TestParserJavaScriptLongMalformedLineRemainsConservative(t *testing.T) {
	line := "const value = (" + strings.Repeat("x", 256*1024) + "\n"
	file := source.File{
		Reference: source.Reference{Path: "long.js", StartLine: 1, EndLine: 1},
		Language:  source.LanguageJavaScript,
		Content:   []byte(line),
	}

	symbols, err := lineparser.NewParser().Parse(context.Background(), file)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(symbols) != 0 {
		t.Fatalf("Parse() symbols = %#v, want no invented malformed declaration", symbols)
	}
}

func TestParserJavaScriptIgnoresDeclarationsInsideMultilineCommentsAndTemplates(t *testing.T) {
	file := source.File{
		Reference: source.Reference{Path: "src/app.js", StartLine: 1, EndLine: 12},
		Language:  source.LanguageJavaScript,
		Content: []byte("" +
			"/*\n" +
			"function commentedOut() {}\n" +
			"*/\n" +
			"const source = `first\n" +
			"class TemplateOnly {}\n" +
			"${function expressionOnly() {}}\n" +
			"`\n" +
			"const blockMarker = \"/*\"\n" +
			"const templateMarker = '`'\n" +
			"function visible() {}\n" +
			"/* trailing comment */\n" +
			"class AlsoVisible {}\n"),
	}

	symbols, err := lineparser.NewParser().Parse(context.Background(), file)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	want := []source.Symbol{
		{Name: "visible", Kind: "function", Signature: "function visible() {}", Reference: source.Reference{Path: "src/app.js", StartLine: 10, EndLine: 10}},
		{Name: "AlsoVisible", Kind: "class", Signature: "class AlsoVisible {}", Reference: source.Reference{Path: "src/app.js", StartLine: 12, EndLine: 12}},
	}
	assertSymbols(t, symbols, want)
}

func TestParserJavaScriptRejectsPartialMalformedDeclarations(t *testing.T) {
	file := source.File{
		Reference: source.Reference{Path: "src/malformed.js", StartLine: 1, EndLine: 5},
		Language:  source.LanguageJavaScript,
		Content: []byte("" +
			"function half-name() {}\n" +
			"class Broken.member {}\n" +
			"const invalidParameter = (return) => 1\n" +
			"const missingParameter = (value,,other) => 1\n" +
			"function visible() {}\n"),
	}

	symbols, err := lineparser.NewParser().Parse(context.Background(), file)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	want := []source.Symbol{
		{Name: "visible", Kind: "function", Signature: "function visible() {}", Reference: source.Reference{Path: "src/malformed.js", StartLine: 5, EndLine: 5}},
	}
	assertSymbols(t, symbols, want)
}

func TestParserJavaScriptMalformedBlockStructureFailsClosed(t *testing.T) {
	file := source.File{
		Reference: source.Reference{Path: "src/unbalanced.js", StartLine: 1, EndLine: 2},
		Language:  source.LanguageJavaScript,
		Content:   []byte("}\nfunction inventedAfterMalformedBlock() {}\n"),
	}

	symbols, err := lineparser.NewParser().Parse(context.Background(), file)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(symbols) != 0 {
		t.Fatalf("Parse() symbols = %#v, want malformed lexical structure to fail closed", symbols)
	}
}

func TestParserJavaScriptFindsOnlySyntacticTopLevelDeclarations(t *testing.T) {
	lines := []string{
		"if (ready) {",
		"const hiddenArrow = () => {}",
		"function hiddenFunction() {}",
		"}",
		"function outer() {",
		"function nestedFunction() {}",
		"}",
		"class Container {",
		"const nestedBinding = function() {}",
		"}",
		"const objectOnly = { formatter: () => ({ value: 1 }) }",
		"const divisionOnly = total / divisor",
		"export const View = (props) => <main style={{ color: 'red' }}>{props.value}</main>",
		"const braceString = '{ not a block }'",
		"const braceTemplate = `{ not a block }`",
		"/* { not a block } */",
		"if (/[{]/.test(value)) {",
		"function regexBlockHidden() {}",
		"}",
		"export const Fragment = () => <><span /></>",
		"function visible() {}",
	}
	file := source.File{
		Reference: source.Reference{Path: "src/blocks.jsx", StartLine: 1, EndLine: len(lines)},
		Language:  source.LanguageJavaScript,
		Content:   []byte(strings.Join(lines, "\n") + "\n"),
	}

	symbols, err := lineparser.NewParser().Parse(context.Background(), file)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	want := []source.Symbol{
		{Name: "outer", Kind: "function", Signature: lines[4], Reference: source.Reference{Path: "src/blocks.jsx", StartLine: 5, EndLine: 5}},
		{Name: "Container", Kind: "class", Signature: lines[7], Reference: source.Reference{Path: "src/blocks.jsx", StartLine: 8, EndLine: 8}},
		{Name: "View", Kind: "function", Signature: lines[12], Reference: source.Reference{Path: "src/blocks.jsx", StartLine: 13, EndLine: 13}},
		{Name: "Fragment", Kind: "function", Signature: lines[19], Reference: source.Reference{Path: "src/blocks.jsx", StartLine: 20, EndLine: 20}},
		{Name: "visible", Kind: "function", Signature: lines[20], Reference: source.Reference{Path: "src/blocks.jsx", StartLine: 21, EndLine: 21}},
	}
	assertSymbols(t, symbols, want)
}

func TestParserJavaScriptValidatesCompleteFunctionParameters(t *testing.T) {
	invalidLines := []string{
		"function directOpen(",
		"function directReserved(return) {}",
		"function directStrict(interface) {}",
		"function directDuplicate(value, value) {}",
		"const anonymousOpen = function(",
		"const anonymousReserved = function(return) {}",
		"const anonymousEmpty = function(, value) {}",
		"export const duplicateExpression = function(a, a) {}",
		"const namedEmpty = function named(, value) {}",
		"const reservedNamed = function return(value) {}",
		"const duplicateArrow = (a, a) => 0",
		"export const interface = function() {}",
		"const strictArrow = (interface) => 0",
		"function interface() {}",
		"function strictEval(eval) {}",
		"const strictArguments = (arguments) => 0",
		"const package = function() {}",
		"function bodyless()",
		"const bodylessExpression = function()",
		"const bodylessArrow = () =>",
	}
	for _, line := range invalidLines {
		t.Run(line, func(t *testing.T) {
			file := source.File{
				Reference: source.Reference{Path: "src/invalid-parameters.js", StartLine: 1, EndLine: 1},
				Language:  source.LanguageJavaScript,
				Content:   []byte(line + "\n"),
			}
			symbols, err := lineparser.NewParser().Parse(context.Background(), file)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if len(symbols) != 0 {
				t.Fatalf("Parse(%q) symbols = %#v, want none", line, symbols)
			}
		})
	}

	validLines := []string{
		"function direct(value, other) {}",
		"const anonymous = function(value) {}",
		"const namedBinding = function named(value) {}",
		"const arrow = (value, other) => 0",
		"const rest = (...values) => values.length",
	}
	file := source.File{
		Reference: source.Reference{Path: "src/parameters.js", StartLine: 1, EndLine: len(validLines)},
		Language:  source.LanguageJavaScript,
		Content:   []byte(strings.Join(validLines, "\n") + "\n"),
	}

	symbols, err := lineparser.NewParser().Parse(context.Background(), file)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	want := []source.Symbol{
		{Name: "direct", Kind: "function", Signature: validLines[0], Reference: source.Reference{Path: "src/parameters.js", StartLine: 1, EndLine: 1}},
		{Name: "anonymous", Kind: "function", Signature: validLines[1], Reference: source.Reference{Path: "src/parameters.js", StartLine: 2, EndLine: 2}},
		{Name: "namedBinding", Kind: "function", Signature: validLines[2], Reference: source.Reference{Path: "src/parameters.js", StartLine: 3, EndLine: 3}},
		{Name: "arrow", Kind: "function", Signature: validLines[3], Reference: source.Reference{Path: "src/parameters.js", StartLine: 4, EndLine: 4}},
		{Name: "rest", Kind: "function", Signature: validLines[4], Reference: source.Reference{Path: "src/parameters.js", StartLine: 5, EndLine: 5}},
	}
	assertSymbols(t, symbols, want)
}

func TestParserJavaScriptRegexLiteralsDoNotCorruptLexicalState(t *testing.T) {
	patterns := []struct {
		name    string
		pattern string
	}{
		{name: "review regression", pattern: "`"},
		{name: "escaped markers", pattern: "\\`\\{\\}\\/\\*"},
		{name: "character class", pattern: "[`{}\\/\\*]"},
	}
	for _, test := range patterns {
		t.Run(test.name, func(t *testing.T) {
			lines := []string{
				"const marker = /" + test.pattern + "/",
				"const source = `first",
				"function fake() {}",
				"class FakeClass {}",
				"`",
				"function real() {}",
			}
			file := source.File{
				Reference: source.Reference{Path: "src/regex.js", StartLine: 1, EndLine: len(lines)},
				Language:  source.LanguageJavaScript,
				Content:   []byte(strings.Join(lines, "\n") + "\n"),
			}

			symbols, err := lineparser.NewParser().Parse(context.Background(), file)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			want := []source.Symbol{
				{Name: "real", Kind: "function", Signature: lines[5], Reference: source.Reference{Path: "src/regex.js", StartLine: 6, EndLine: 6}},
			}
			assertSymbols(t, symbols, want)
		})
	}
}

func TestParserJavaScriptRegexAfterControlHeaderDoesNotCorruptTemplateState(t *testing.T) {
	lines := []string{
		"const groupedDivision = (total) / divisor",
		"call(value) / divisor",
		"if (check(ready)) /`/.test(value)",
		"const source = `first",
		"function fake() {}",
		"`",
		"function real() {}",
	}
	file := source.File{
		Reference: source.Reference{Path: "src/control-regex.js", StartLine: 1, EndLine: len(lines)},
		Language:  source.LanguageJavaScript,
		Content:   []byte(strings.Join(lines, "\n") + "\n"),
	}

	symbols, err := lineparser.NewParser().Parse(context.Background(), file)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	want := []source.Symbol{
		{Name: "real", Kind: "function", Signature: lines[6], Reference: source.Reference{Path: "src/control-regex.js", StartLine: 7, EndLine: 7}},
	}
	assertSymbols(t, symbols, want)
}

func TestParserJavaScriptRegexAfterControlBlockDoesNotCorruptTemplateState(t *testing.T) {
	lines := []string{
		"if (ready) {} /`/.test(value)",
		"const source = `first",
		"function fake() {}",
		"`",
		"function real() {}",
	}
	file := source.File{
		Reference: source.Reference{Path: "src/control-block-regex.js", StartLine: 1, EndLine: len(lines)},
		Language:  source.LanguageJavaScript,
		Content:   []byte(strings.Join(lines, "\n") + "\n"),
	}

	symbols, err := lineparser.NewParser().Parse(context.Background(), file)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	want := []source.Symbol{
		{Name: "real", Kind: "function", Signature: lines[4], Reference: source.Reference{Path: "src/control-block-regex.js", StartLine: 5, EndLine: 5}},
	}
	assertSymbols(t, symbols, want)
}

func TestParserJavaScriptRegexAfterStatementBlockDoesNotCorruptTemplateState(t *testing.T) {
	for _, prefix := range []string{
		"{} /`/.test(value)",
		"  {} /* bridge */ /`/.test(value)",
	} {
		t.Run(prefix, func(t *testing.T) {
			lines := []string{
				prefix,
				"const source = `first",
				"function fake() {}",
				"`",
				"function real() {}",
			}
			file := source.File{
				Reference: source.Reference{Path: "src/statement-block-regex.js", StartLine: 1, EndLine: len(lines)},
				Language:  source.LanguageJavaScript,
				Content:   []byte(strings.Join(lines, "\n") + "\n"),
			}

			symbols, err := lineparser.NewParser().Parse(context.Background(), file)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			want := []source.Symbol{
				{Name: "real", Kind: "function", Signature: lines[4], Reference: source.Reference{Path: "src/statement-block-regex.js", StartLine: 5, EndLine: 5}},
			}
			assertSymbols(t, symbols, want)
		})
	}
}

func TestParserJavaScriptPreservesObjectDivisionContext(t *testing.T) {
	lines := []string{
		"const ratio = {} / divisor",
		"({ value: 1 }) / divisor",
		"function real() {}",
	}
	file := source.File{
		Reference: source.Reference{Path: "src/object-division.js", StartLine: 1, EndLine: len(lines)},
		Language:  source.LanguageJavaScript,
		Content:   []byte(strings.Join(lines, "\n") + "\n"),
	}

	symbols, err := lineparser.NewParser().Parse(context.Background(), file)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	want := []source.Symbol{
		{Name: "real", Kind: "function", Signature: lines[2], Reference: source.Reference{Path: "src/object-division.js", StartLine: 3, EndLine: 3}},
	}
	assertSymbols(t, symbols, want)
}

func TestParserJavaScriptIgnoresDeclarationTextInsideMultilineJSX(t *testing.T) {
	lines := []string{
		"<section data-label=\"</section>\">",
		"{ready ? 'function expressionText() {}' : null}",
		"const fake = () => <span />",
		"</section>",
		"function real() {}",
	}
	file := source.File{
		Reference: source.Reference{Path: "src/multiline.jsx", StartLine: 1, EndLine: len(lines)},
		Language:  source.LanguageJavaScript,
		Content:   []byte(strings.Join(lines, "\n") + "\n"),
	}

	symbols, err := lineparser.NewParser().Parse(context.Background(), file)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	want := []source.Symbol{
		{Name: "real", Kind: "function", Signature: lines[4], Reference: source.Reference{Path: "src/multiline.jsx", StartLine: 5, EndLine: 5}},
	}
	assertSymbols(t, symbols, want)
}

func TestParserJavaScriptRejectsMalformedArrowBodyStarters(t *testing.T) {
	validLines := []string{
		"const expressionBody = () => value",
		"const blockBody = () => { return value }",
		"const jsxBody = () => <span />",
	}
	file := source.File{
		Reference: source.Reference{Path: "src/arrows.jsx", StartLine: 1, EndLine: len(validLines)},
		Language:  source.LanguageJavaScript,
		Content:   []byte(strings.Join(validLines, "\n") + "\n"),
	}

	symbols, err := lineparser.NewParser().Parse(context.Background(), file)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	want := []source.Symbol{
		{Name: "expressionBody", Kind: "function", Signature: validLines[0], Reference: source.Reference{Path: "src/arrows.jsx", StartLine: 1, EndLine: 1}},
		{Name: "blockBody", Kind: "function", Signature: validLines[1], Reference: source.Reference{Path: "src/arrows.jsx", StartLine: 2, EndLine: 2}},
		{Name: "jsxBody", Kind: "function", Signature: validLines[2], Reference: source.Reference{Path: "src/arrows.jsx", StartLine: 3, EndLine: 3}},
	}
	assertSymbols(t, symbols, want)

	invalidLines := []string{
		"const doubleArrow = () => =>",
		"const closeParen = () => )",
		"const throwBody = () => throw value",
	}
	for _, line := range invalidLines {
		t.Run(line, func(t *testing.T) {
			invalidFile := source.File{
				Reference: source.Reference{Path: "src/invalid-arrow.js", StartLine: 1, EndLine: 1},
				Language:  source.LanguageJavaScript,
				Content:   []byte(line + "\n"),
			}
			got, parseErr := lineparser.NewParser().Parse(context.Background(), invalidFile)
			if parseErr != nil {
				t.Fatalf("Parse() error = %v", parseErr)
			}
			if len(got) != 0 {
				t.Fatalf("Parse(%q) symbols = %#v, want none", line, got)
			}
		})
	}
}

func TestParserJavaScriptArrowBodyUsesConservativeStarterSubset(t *testing.T) {
	invalidLines := []string{
		"const lessThan = () => < value",
		"const inBody = () => in value",
		"const instanceBody = () => instanceof Type",
		"const extendsBody = () => extends Type",
		"const positive = () => +",
		"const negated = () => !",
		"const inverted = () => ~",
		"const constructed = () => new",
		"const typed = () => typeof",
		"const deleted = () => delete",
		"const awaited = async () => await",
		"const functionBody = () => function",
		"const classBody = () => class",
		"const superBody = () => super",
		"const regexBody = () => /unterminated",
		"const jsxBody = () => <Tag",
	}
	for _, line := range invalidLines {
		t.Run("invalid "+line, func(t *testing.T) {
			file := source.File{
				Reference: source.Reference{Path: "src/invalid-starter.jsx", StartLine: 1, EndLine: 1},
				Language:  source.LanguageJavaScript,
				Content:   []byte(line + "\n"),
			}
			symbols, err := lineparser.NewParser().Parse(context.Background(), file)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if len(symbols) != 0 {
				t.Fatalf("Parse(%q) symbols = %#v, want none", line, symbols)
			}
		})
	}

	validLines := []struct {
		line string
		name string
	}{
		{line: "const positive = () => +value", name: "positive"},
		{line: "const negated = () => !ready", name: "negated"},
		{line: "const inverted = () => ~mask", name: "inverted"},
		{line: "const constructed = () => new Type()", name: "constructed"},
		{line: "const typed = () => typeof value", name: "typed"},
		{line: "const deleted = () => delete target.value", name: "deleted"},
		{line: "const awaited = async () => await load()", name: "awaited"},
		{line: "const functionBody = () => function() {}", name: "functionBody"},
		{line: "const classBody = () => class {}", name: "classBody"},
		{line: "const truthy = () => true", name: "truthy"},
		{line: "const self = () => this", name: "self"},
		{line: "const regexBody = () => /closed/.test(value)", name: "regexBody"},
		{line: "const jsxBody = () => <Tag />", name: "jsxBody"},
	}
	for _, test := range validLines {
		t.Run("valid "+test.name, func(t *testing.T) {
			file := source.File{
				Reference: source.Reference{Path: "src/valid-starter.jsx", StartLine: 1, EndLine: 1},
				Language:  source.LanguageJavaScript,
				Content:   []byte(test.line + "\n"),
			}
			symbols, err := lineparser.NewParser().Parse(context.Background(), file)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			want := []source.Symbol{{
				Name: test.name, Kind: "function", Signature: test.line,
				Reference: source.Reference{Path: "src/valid-starter.jsx", StartLine: 1, EndLine: 1},
			}}
			assertSymbols(t, symbols, want)
		})
	}
}

func TestParserJavaScriptAwaitRequiresAsyncArrowAndYieldFailsClosed(t *testing.T) {
	invalidLines := []string{
		"const nonAsync = () => await load()",
		"const yielded = () => yield value",
		"const asyncParameter = async => await load()",
	}
	for _, line := range invalidLines {
		t.Run("invalid "+line, func(t *testing.T) {
			file := source.File{
				Reference: source.Reference{Path: "src/arrow-context.js", StartLine: 1, EndLine: 1},
				Language:  source.LanguageJavaScript,
				Content:   []byte(line + "\n"),
			}
			symbols, err := lineparser.NewParser().Parse(context.Background(), file)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if len(symbols) != 0 {
				t.Fatalf("Parse(%q) symbols = %#v, want none", line, symbols)
			}
		})
	}

	validLines := []struct {
		line string
		name string
	}{
		{line: "const load = async () => await loadValue()", name: "load"},
		{line: "const mapped = async value => await transform(value)", name: "mapped"},
		{line: "const asyncParameter = async => async", name: "asyncParameter"},
	}
	for _, test := range validLines {
		t.Run("valid "+test.line, func(t *testing.T) {
			file := source.File{
				Reference: source.Reference{Path: "src/async-arrow.js", StartLine: 1, EndLine: 1},
				Language:  source.LanguageJavaScript,
				Content:   []byte(test.line + "\n"),
			}
			symbols, err := lineparser.NewParser().Parse(context.Background(), file)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			want := []source.Symbol{{
				Name: test.name, Kind: "function", Signature: test.line,
				Reference: source.Reference{Path: "src/async-arrow.js", StartLine: 1, EndLine: 1},
			}}
			assertSymbols(t, symbols, want)
		})
	}
}

func TestParserJavaScriptDelimiterOrderMustBeProperlyNested(t *testing.T) {
	invalidLines := []string{
		"const crossedBracket = () => ([)]",
		"const crossedBrace = () => ({)}",
	}
	for _, line := range invalidLines {
		t.Run(line, func(t *testing.T) {
			file := source.File{
				Reference: source.Reference{Path: "src/crossed.js", StartLine: 1, EndLine: 1},
				Language:  source.LanguageJavaScript,
				Content:   []byte(line + "\n"),
			}
			symbols, err := lineparser.NewParser().Parse(context.Background(), file)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if len(symbols) != 0 {
				t.Fatalf("Parse(%q) symbols = %#v, want crossed delimiters to fail closed", line, symbols)
			}
		})
	}

	line := "const nested = () => ([{ value: [call()] }])"
	file := source.File{
		Reference: source.Reference{Path: "src/nested.js", StartLine: 1, EndLine: 1},
		Language:  source.LanguageJavaScript,
		Content:   []byte(line + "\n"),
	}
	symbols, err := lineparser.NewParser().Parse(context.Background(), file)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	want := []source.Symbol{{
		Name: "nested", Kind: "function", Signature: line,
		Reference: source.Reference{Path: "src/nested.js", StartLine: 1, EndLine: 1},
	}}
	assertSymbols(t, symbols, want)
}

func TestParserJavaScriptRejectsCandidateThatMakesLexicalStateUncertain(t *testing.T) {
	lines := []string{
		"const validRegex = () => /closed/.test(value)",
		"const unterminatedRegex = () => /unterminated",
		"function inventedAfterMalformedRegex() {}",
	}
	file := source.File{
		Reference: source.Reference{Path: "src/uncertain.js", StartLine: 1, EndLine: len(lines)},
		Language:  source.LanguageJavaScript,
		Content:   []byte(strings.Join(lines, "\n") + "\n"),
	}

	symbols, err := lineparser.NewParser().Parse(context.Background(), file)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	want := []source.Symbol{
		{Name: "validRegex", Kind: "function", Signature: lines[0], Reference: source.Reference{Path: "src/uncertain.js", StartLine: 1, EndLine: 1}},
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

type cancelAfterErrChecksContext struct {
	context.Context
	cancelAfter int
	checks      int
}

func (ctx *cancelAfterErrChecksContext) Err() error {
	ctx.checks++
	if ctx.checks >= ctx.cancelAfter {
		return context.Canceled
	}
	return nil
}

func TestParserJavaScriptChecksCancellationWithinLongLexicalScan(t *testing.T) {
	ctx := &cancelAfterErrChecksContext{Context: context.Background(), cancelAfter: 4}
	_, err := lineparser.NewParser().Parse(ctx, source.File{
		Reference: source.Reference{Path: "long.js", StartLine: 1, EndLine: 1},
		Language:  source.LanguageJavaScript,
		Content:   []byte("const value = " + strings.Repeat("x", 16*1024)),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Parse() error = %v after %d checks, want context.Canceled during lexical scan", err, ctx.checks)
	}
}

func TestParserJavaScriptChecksCancellationDuringJSXValidation(t *testing.T) {
	ctx := &cancelAfterErrChecksContext{Context: context.Background(), cancelAfter: 5}
	_, err := lineparser.NewParser().Parse(ctx, source.File{
		Reference: source.Reference{Path: "view.jsx", StartLine: 1, EndLine: 1},
		Language:  source.LanguageJavaScript,
		Content:   []byte("const View = () => <main />"),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Parse() error = %v after %d checks, want context.Canceled during JSX validation", err, ctx.checks)
	}
}

func TestParserJavaScriptJSXValidationWorkIsLinearAndDelimiterDepthIsBounded(t *testing.T) {
	content := "const View = () => <main>" + strings.Repeat("x", 32*1024) + "</main>"
	ctx := &cancelAfterErrChecksContext{Context: context.Background(), cancelAfter: 1 << 30}
	symbols, err := lineparser.NewParser().Parse(ctx, source.File{
		Reference: source.Reference{Path: "large-view.jsx", StartLine: 1, EndLine: 1},
		Language:  source.LanguageJavaScript,
		Content:   []byte(content),
	})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(symbols) != 1 || symbols[0].Name != "View" {
		t.Fatalf("Parse() symbols = %#v, want View", symbols)
	}
	maximumChecks := 2*(len(content)/javaScriptCancellationTestStride+2) + 4
	if ctx.checks > maximumChecks {
		t.Fatalf("Parse() made %d context checks, want at most %d linear checks", ctx.checks, maximumChecks)
	}

	tooDeep := "const Deep = () => <main>{" + strings.Repeat("(", 257) + "value" + strings.Repeat(")", 257) + "}</main>"
	deepSymbols, err := lineparser.NewParser().Parse(context.Background(), source.File{
		Reference: source.Reference{Path: "deep-view.jsx", StartLine: 1, EndLine: 1},
		Language:  source.LanguageJavaScript,
		Content:   []byte(tooDeep),
	})
	if err != nil {
		t.Fatalf("Parse(deep) error = %v", err)
	}
	if len(deepSymbols) != 0 {
		t.Fatalf("Parse(deep) symbols = %#v, want bounded fail-closed result", deepSymbols)
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
