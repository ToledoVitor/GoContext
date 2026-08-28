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
	validLine  func(string) bool
}

type javaScriptLexicalState struct {
	blockComment bool
	template     bool
	stringQuote  byte
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

var javaScriptPatterns = []declarationPattern{
	{
		expression: regexp.MustCompile(`^(?:export\s+(?:default\s+)?)?(?:async\s+)?function(?:\s*\*\s*|\s+)([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`),
		kind:       "function",
	},
	{
		expression: regexp.MustCompile(`^(?:export\s+(?:default\s+)?)?class\s+([A-Za-z_$][A-Za-z0-9_$]*)(?:\s+extends\s+[A-Za-z_$][A-Za-z0-9_$]*(?:\.[A-Za-z_$][A-Za-z0-9_$]*)*)?\s*\{`),
		kind:       "class",
	},
	{
		expression: regexp.MustCompile(`^(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*(?:async\s+)?(?:function(?:\s*\*)?(?:\s+[A-Za-z_$][A-Za-z0-9_$]*)?\s*\(|(?:[A-Za-z_$][A-Za-z0-9_$]*|\([^)]*\))\s*=>)`),
		kind:       "function",
		validLine:  validJavaScriptFunctionInitializer,
	},
}

var javaScriptReservedIdentifiers = map[string]struct{}{
	"await": {}, "break": {}, "case": {}, "catch": {}, "class": {}, "const": {},
	"continue": {}, "debugger": {}, "default": {}, "delete": {}, "do": {}, "else": {},
	"enum": {}, "export": {}, "extends": {}, "false": {}, "finally": {}, "for": {},
	"function": {}, "if": {}, "import": {}, "in": {}, "instanceof": {}, "let": {},
	"new": {}, "null": {}, "return": {}, "static": {}, "super": {}, "switch": {},
	"this": {}, "throw": {}, "true": {}, "try": {}, "typeof": {}, "var": {},
	"void": {}, "while": {}, "with": {}, "yield": {},
}

// Parser provides initial declaration discovery while Tree-sitter is pending.
type Parser struct{}

// NewParser creates a parser for JavaScript, Python, and TypeScript source files.
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
	var javaScriptState javaScriptLexicalState
	for index, rawLine := range lines {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		line := strings.TrimSuffix(rawLine, "\r")
		lineStartsInCode := true
		if file.Language == source.LanguageJavaScript {
			lineStartsInCode = javaScriptState.inCode()
			javaScriptState.consume(line)
		}
		if !lineStartsInCode || line == "" || line != strings.TrimLeft(line, " \t") {
			continue
		}

		for _, pattern := range patterns {
			matches := pattern.expression.FindStringSubmatch(line)
			if len(matches) != 2 {
				continue
			}
			if file.Language == source.LanguageJavaScript {
				if _, reserved := javaScriptReservedIdentifiers[matches[1]]; reserved {
					continue
				}
			}
			if pattern.validLine != nil && !pattern.validLine(line) {
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

func (state *javaScriptLexicalState) inCode() bool {
	return !state.blockComment && !state.template && state.stringQuote == 0
}

// consume tracks only the multiline lexical forms needed to prevent declarations
// inside comments and literals from becoming symbol boundaries. Template
// substitutions remain opaque intentionally: this line parser prefers false
// negatives to inventing declarations without a JavaScript grammar.
func (state *javaScriptLexicalState) consume(line string) {
	escaped := false
	for index := 0; index < len(line); index++ {
		character := line[index]

		switch {
		case state.blockComment:
			if character == '*' && index+1 < len(line) && line[index+1] == '/' {
				state.blockComment = false
				index++
			}
		case state.template:
			if escaped {
				escaped = false
				continue
			}
			if character == '\\' {
				escaped = true
				continue
			}
			if character == '`' {
				state.template = false
			}
		case state.stringQuote != 0:
			if escaped {
				escaped = false
				continue
			}
			if character == '\\' {
				escaped = true
				continue
			}
			if character == state.stringQuote {
				state.stringQuote = 0
			}
		default:
			switch character {
			case '/', '\'', '"', '`':
				switch character {
				case '/':
					if index+1 >= len(line) {
						continue
					}
					switch line[index+1] {
					case '/':
						return
					case '*':
						state.blockComment = true
						index++
					}
				case '\'', '"':
					state.stringQuote = character
				case '`':
					state.template = true
				}
			}
		}
	}

	if state.stringQuote != 0 && !escaped {
		state.stringQuote = 0
	}
}

func validJavaScriptFunctionInitializer(line string) bool {
	delimiter := strings.IndexByte(line, '=')
	if delimiter < 0 {
		return false
	}
	right := strings.TrimSpace(line[delimiter+1:])
	if hasJavaScriptKeywordPrefix(right, "async") {
		remainder := strings.TrimSpace(right[len("async"):])
		if !strings.HasPrefix(remainder, "=>") {
			right = remainder
		}
	}
	if hasJavaScriptKeywordPrefix(right, "function") || right == "function" {
		remainder := strings.TrimSpace(right[len("function"):])
		if strings.HasPrefix(remainder, "*") {
			remainder = strings.TrimSpace(remainder[1:])
		}
		if strings.HasPrefix(remainder, "(") {
			return true
		}
		name, rest := leadingJavaScriptIdentifier(remainder)
		if name == "" || javaScriptIdentifierReserved(name) {
			return false
		}
		return strings.HasPrefix(strings.TrimSpace(rest), "(")
	}
	if strings.HasPrefix(right, "(") {
		return validJavaScriptParenthesizedArrowParameters(right)
	}
	parameter, rest := leadingJavaScriptIdentifier(right)
	return parameter != "" && !javaScriptIdentifierReserved(parameter) &&
		strings.HasPrefix(strings.TrimSpace(rest), "=>")
}

func validJavaScriptParenthesizedArrowParameters(value string) bool {
	closing := strings.IndexByte(value, ')')
	if closing < 0 || !strings.HasPrefix(strings.TrimSpace(value[closing+1:]), "=>") {
		return false
	}
	parameters := strings.TrimSpace(value[1:closing])
	if parameters == "" {
		return true
	}
	for _, parameter := range strings.Split(parameters, ",") {
		parameter = strings.TrimSpace(parameter)
		parameter = strings.TrimPrefix(parameter, "...")
		identifier, rest := leadingJavaScriptIdentifier(parameter)
		if identifier == "" || rest != "" || javaScriptIdentifierReserved(identifier) {
			return false
		}
	}
	return true
}

func hasJavaScriptKeywordPrefix(value, keyword string) bool {
	if !strings.HasPrefix(value, keyword) || len(value) == len(keyword) {
		return false
	}
	next := value[len(keyword)]
	return next == ' ' || next == '\t' || next == '\r' || next == '\n' || next == '*' || next == '('
}

func leadingJavaScriptIdentifier(value string) (string, string) {
	end := 0
	for end < len(value) {
		character := value[end]
		if !(character == '$' || character == '_' || character >= 'A' && character <= 'Z' ||
			character >= 'a' && character <= 'z' || end > 0 && character >= '0' && character <= '9') {
			break
		}
		end++
	}
	return value[:end], value[end:]
}

func javaScriptIdentifierReserved(identifier string) bool {
	_, reserved := javaScriptReservedIdentifiers[identifier]
	return reserved
}

func patternsForLanguage(language source.Language) ([]declarationPattern, error) {
	switch language {
	case source.LanguageJavaScript:
		return javaScriptPatterns, nil
	case source.LanguagePython:
		return pythonPatterns, nil
	case source.LanguageTypeScript:
		return typeScriptPatterns, nil
	default:
		return nil, ErrUnsupportedLanguage
	}
}

var _ ingest.Parser = (*Parser)(nil)
