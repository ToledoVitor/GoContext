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
		validLine:  validJavaScriptFunctionDeclaration,
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
	"arguments": {}, "await": {}, "break": {}, "case": {}, "catch": {}, "class": {}, "const": {},
	"continue": {}, "debugger": {}, "default": {}, "delete": {}, "do": {}, "else": {},
	"enum": {}, "eval": {}, "export": {}, "extends": {}, "false": {}, "finally": {}, "for": {},
	"function": {}, "if": {}, "implements": {}, "import": {}, "in": {}, "instanceof": {}, "interface": {}, "let": {},
	"new": {}, "null": {}, "package": {}, "private": {}, "protected": {}, "public": {}, "return": {},
	"static": {}, "super": {}, "switch": {},
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
	javaScriptState := newJavaScriptLexicalState()
	for index, rawLine := range lines {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		line := strings.TrimSuffix(rawLine, "\r")
		lineStartsAtTopLevel := true
		lineRemainsTrustworthy := true
		if file.Language == source.LanguageJavaScript {
			lineStartsAtTopLevel = javaScriptState.atTopLevel()
			if err := javaScriptState.consume(ctx, line); err != nil {
				return nil, err
			}
			lineRemainsTrustworthy = javaScriptState.trustworthy()
		}
		if !lineStartsAtTopLevel || !lineRemainsTrustworthy || line == "" || line != strings.TrimLeft(line, " \t") {
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

func validJavaScriptFunctionDeclaration(line string) bool {
	function := strings.Index(line, "function")
	return function >= 0 && validJavaScriptFunctionSyntax(line[function:], true)
}

func validJavaScriptFunctionInitializer(line string) bool {
	delimiter := strings.IndexByte(line, '=')
	if delimiter < 0 {
		return false
	}
	right := strings.TrimSpace(line[delimiter+1:])
	if hasJavaScriptKeywordPrefix(right, "async") {
		remainder := strings.TrimSpace(right[len("async"):])
		if hasJavaScriptKeywordPrefix(remainder, "function") || remainder == "function" {
			return validJavaScriptFunctionSyntax(remainder, false)
		}
		if validJavaScriptArrowFunction(remainder) {
			return true
		}
		return validJavaScriptArrowFunction(right)
	}
	if hasJavaScriptKeywordPrefix(right, "function") || right == "function" {
		return validJavaScriptFunctionSyntax(right, false)
	}
	return validJavaScriptArrowFunction(right)
}

func validJavaScriptFunctionSyntax(value string, requireName bool) bool {
	if !strings.HasPrefix(value, "function") {
		return false
	}
	remainder := strings.TrimSpace(value[len("function"):])
	if strings.HasPrefix(remainder, "*") {
		remainder = strings.TrimSpace(remainder[1:])
	}

	if requireName || !strings.HasPrefix(remainder, "(") {
		name, rest := leadingJavaScriptIdentifier(remainder)
		if !validJavaScriptIdentifier(name) {
			return false
		}
		remainder = strings.TrimSpace(rest)
	}
	tail, valid := validJavaScriptParameterList(remainder)
	if !valid {
		return false
	}
	tail = strings.TrimSpace(tail)
	return strings.HasPrefix(tail, "{")
}

func validJavaScriptArrowFunction(value string) bool {
	if strings.HasPrefix(value, "(") {
		tail, valid := validJavaScriptParameterList(value)
		return valid && validJavaScriptArrowTail(tail)
	}
	parameter, rest := leadingJavaScriptIdentifier(value)
	return validJavaScriptIdentifier(parameter) && validJavaScriptArrowTail(rest)
}

func validJavaScriptArrowTail(value string) bool {
	tail := strings.TrimSpace(value)
	if !strings.HasPrefix(tail, "=>") {
		return false
	}
	body := strings.TrimSpace(tail[len("=>"):])
	if body == "" || strings.HasPrefix(body, "=>") || strings.HasPrefix(body, "//") ||
		strings.HasPrefix(body, "/*") {
		return false
	}
	return validJavaScriptArrowBodyStarter(body)
}

// validJavaScriptArrowBodyStarter deliberately recognizes a small expression
// prefix subset. The lexical pass validates delimiters and literals; this
// guard prevents statement/binary keywords and incomplete unary constructs
// from becoming invented function boundaries without implementing an AST.
func validJavaScriptArrowBodyStarter(body string) bool {
	value := strings.TrimLeft(body, " \t\r\n")
	for value != "" {
		first := value[0]
		switch {
		case first == '+' || first == '-' || first == '!' || first == '~':
			value = strings.TrimLeft(value[1:], " \t\r\n")
			continue
		case first == '<':
			return completeJavaScriptJSXExpression(value)
		case first == '/' || first == '\'' || first == '"' || first == '`' ||
			first == '(' || first == '[' || first == '{':
			return true
		case first >= '0' && first <= '9':
			return true
		case isJavaScriptIdentifierStart(first):
			keyword, rest := leadingJavaScriptIdentifier(value)
			switch keyword {
			case "await", "delete", "new", "typeof", "void", "yield":
				value = strings.TrimLeft(rest, " \t\r\n")
				continue
			case "function":
				return validJavaScriptFunctionSyntax(value, false)
			case "class":
				return validJavaScriptClassExpressionSyntax(value)
			case "false", "null", "this", "true":
				return true
			default:
				return validJavaScriptIdentifier(keyword)
			}
		default:
			return false
		}
	}
	return false
}

func validJavaScriptClassExpressionSyntax(value string) bool {
	if !strings.HasPrefix(value, "class") {
		return false
	}
	remainder := strings.TrimLeft(value[len("class"):], " \t\r\n")
	if strings.HasPrefix(remainder, "{") {
		return true
	}

	name, rest := leadingJavaScriptIdentifier(remainder)
	if !validJavaScriptIdentifier(name) {
		return false
	}
	remainder = strings.TrimLeft(rest, " \t\r\n")
	if hasJavaScriptKeywordPrefix(remainder, "extends") {
		remainder = strings.TrimLeft(remainder[len("extends"):], " \t\r\n")
		base, tail := leadingJavaScriptIdentifier(remainder)
		if !validJavaScriptIdentifier(base) {
			return false
		}
		remainder = strings.TrimLeft(tail, " \t\r\n")
		for strings.HasPrefix(remainder, ".") {
			member, tail := leadingJavaScriptIdentifier(remainder[1:])
			if !validJavaScriptIdentifier(member) {
				return false
			}
			remainder = strings.TrimLeft(tail, " \t\r\n")
		}
	}
	return strings.HasPrefix(remainder, "{")
}

func completeJavaScriptJSXExpression(value string) bool {
	if !startsJavaScriptJSX(value, 0) {
		return false
	}
	state := newJavaScriptLexicalState()
	if err := state.consume(context.Background(), value); err != nil {
		return false
	}
	return state.atTopLevel()
}

func validJavaScriptParameterList(value string) (string, bool) {
	if !strings.HasPrefix(value, "(") {
		return "", false
	}
	closing := strings.IndexByte(value, ')')
	if closing < 0 {
		return "", false
	}
	parameters := strings.TrimSpace(value[1:closing])
	if parameters == "" {
		return value[closing+1:], true
	}
	parts := strings.Split(parameters, ",")
	seen := make(map[string]struct{}, len(parts))
	for position, parameter := range parts {
		parameter = strings.TrimSpace(parameter)
		if parameter == "" {
			return "", false
		}
		if strings.HasPrefix(parameter, "...") {
			if position != len(parts)-1 {
				return "", false
			}
			parameter = strings.TrimSpace(parameter[len("..."):])
		}
		identifier, rest := leadingJavaScriptIdentifier(parameter)
		if !validJavaScriptIdentifier(identifier) || rest != "" {
			return "", false
		}
		if _, duplicate := seen[identifier]; duplicate {
			return "", false
		}
		seen[identifier] = struct{}{}
	}
	return value[closing+1:], true
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

func validJavaScriptIdentifier(identifier string) bool {
	if identifier == "" || javaScriptIdentifierReserved(identifier) {
		return false
	}
	parsed, rest := leadingJavaScriptIdentifier(identifier)
	return parsed == identifier && rest == ""
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
