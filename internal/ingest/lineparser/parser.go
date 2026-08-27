// Package lineparser discovers common top-level declarations without an AST.
package lineparser

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/source"
)

var (
	// ErrUnsupportedLanguage reports a file outside this parser's MVP scope.
	ErrUnsupportedLanguage = errors.New("unsupported source language")
	// ErrInvalidReference reports source provenance unsafe for symbol output.
	ErrInvalidReference = errors.New("invalid source reference")
)

type declarationPattern struct {
	expression *regexp.Regexp
	kind       string
}

var pythonPatterns = []declarationPattern{
	{expression: regexp.MustCompile(`^(?:async\s+)?def\s+([A-Za-z_][A-Za-z0-9_]*)`), kind: "function"},
	{expression: regexp.MustCompile(`^class\s+([A-Za-z_][A-Za-z0-9_]*)`), kind: "class"},
}

var typeScriptPatterns = []declarationPattern{
	{expression: regexp.MustCompile(`^(?:(?:export|default|declare|async)\s+)*function\s+([A-Za-z_$][A-Za-z0-9_$]*)`), kind: "function"},
	{expression: regexp.MustCompile(`^(?:(?:export|default|declare|abstract)\s+)*class\s+([A-Za-z_$][A-Za-z0-9_$]*)`), kind: "class"},
	{expression: regexp.MustCompile(`^(?:(?:export|declare)\s+)*interface\s+([A-Za-z_$][A-Za-z0-9_$]*)`), kind: "interface"},
	{expression: regexp.MustCompile(`^(?:(?:export|declare)\s+)*type\s+([A-Za-z_$][A-Za-z0-9_$]*)`), kind: "type"},
	{expression: regexp.MustCompile(`^(?:(?:export|declare|const)\s+)*enum\s+([A-Za-z_$][A-Za-z0-9_$]*)`), kind: "enum"},
}

// Parser provides initial declaration discovery while Tree-sitter is pending.
type Parser struct{}

// NewParser creates a parser for Python and TypeScript source files.
func NewParser() *Parser {
	return &Parser{}
}

// Parse returns common top-level declarations in source order.
func (p *Parser) Parse(ctx context.Context, file source.File) ([]source.Symbol, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !file.Reference.Valid() {
		return nil, ErrInvalidReference
	}

	patterns, err := patternsForLanguage(file.Language)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(file.Content), "\n")
	symbols := make([]source.Symbol, 0)
	for index, rawLine := range lines {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		line := strings.TrimSuffix(rawLine, "\r")
		if line == "" || line != strings.TrimLeft(line, " \t") {
			continue
		}

		for _, pattern := range patterns {
			matches := pattern.expression.FindStringSubmatch(line)
			if len(matches) != 2 {
				continue
			}

			lineNumber := file.Reference.StartLine + index
			symbols = append(symbols, source.Symbol{
				Name:      matches[1],
				Kind:      pattern.kind,
				Signature: strings.TrimSpace(line),
				Reference: source.Reference{
					Path:      file.Reference.Path,
					StartLine: lineNumber,
					EndLine:   lineNumber,
				},
			})
			break
		}
	}

	return symbols, nil
}

func patternsForLanguage(language source.Language) ([]declarationPattern, error) {
	switch language {
	case source.LanguagePython:
		return pythonPatterns, nil
	case source.LanguageTypeScript:
		return typeScriptPatterns, nil
	default:
		return nil, ErrUnsupportedLanguage
	}
}

var _ ingest.Parser = (*Parser)(nil)
