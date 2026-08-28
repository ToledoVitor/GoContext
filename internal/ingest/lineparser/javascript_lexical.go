package lineparser

import "context"

const javaScriptCancellationStride = 4 << 10
const javaScriptMaximumDelimiterDepth = 256

type javaScriptDelimiterKind uint8

const (
	javaScriptExpressionParen javaScriptDelimiterKind = iota
	javaScriptControlHeaderParen
	javaScriptBracket
	javaScriptGenericBrace
	javaScriptControlBlockBrace
	javaScriptTemplateInterpolationBrace
	javaScriptJSXExpressionBrace
)

// javaScriptLexicalState tracks only the lexical context needed to decide
// whether a declaration starts at syntactic file scope. It is intentionally
// smaller than a JavaScript grammar and fails closed after unsupported nested
// template forms or malformed unterminated literals.
type javaScriptLexicalState struct {
	blockComment          bool
	canStartRegex         bool
	delimiters            [javaScriptMaximumDelimiterDepth]javaScriptDelimiterKind
	delimiterDepth        int
	pendingControlParen   bool
	pendingControlBlock   bool
	stringQuote           byte
	templateRaw           bool
	templateInterpolation bool
	jsxDepth              int
	jsxInTag              bool
	jsxClosingTag         bool
	jsxQuote              byte
	jsxEscaped            bool
	jsxExpression         bool
	uncertain             bool
}

func newJavaScriptLexicalState() *javaScriptLexicalState {
	return &javaScriptLexicalState{canStartRegex: true}
}

func (state *javaScriptLexicalState) atTopLevel() bool {
	return !state.uncertain && !state.blockComment && state.stringQuote == 0 &&
		!state.templateRaw && !state.templateInterpolation && state.jsxDepth == 0 &&
		state.delimiterDepth == 0
}

func (state *javaScriptLexicalState) trustworthy() bool {
	return !state.uncertain
}

func (state *javaScriptLexicalState) consume(ctx context.Context, line string) error {
	if state.uncertain {
		return ctx.Err()
	}

	escaped := false
	regex := false
	regexCharacterClass := false
	seenToken := false
	for index := 0; index < len(line); index++ {
		if index%javaScriptCancellationStride == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}

		character := line[index]
		switch {
		case state.blockComment:
			if character == '*' && index+1 < len(line) && line[index+1] == '/' {
				state.blockComment = false
				index++
			}

		case state.templateRaw:
			if escaped {
				escaped = false
				continue
			}
			if character == '\\' {
				escaped = true
				continue
			}
			if character == '$' && index+1 < len(line) && line[index+1] == '{' {
				if !state.pushDelimiter(javaScriptTemplateInterpolationBrace) {
					return nil
				}
				state.templateRaw = false
				state.templateInterpolation = true
				state.canStartRegex = true
				index++
				continue
			}
			if character == '`' {
				state.templateRaw = false
				state.canStartRegex = false
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
				state.canStartRegex = false
			}

		case regex:
			if escaped {
				escaped = false
				continue
			}
			if character == '\\' {
				escaped = true
				continue
			}
			if regexCharacterClass {
				if character == ']' {
					regexCharacterClass = false
				}
				continue
			}
			if character == '[' {
				regexCharacterClass = true
				continue
			}
			if character == '/' {
				regex = false
				state.canStartRegex = false
			}

		case state.jsxDepth > 0 && !state.jsxExpression:
			state.consumeJSXCharacter(line, &index)

		default:
			if isJavaScriptWhitespace(character) {
				continue
			}
			atLineStart := !seenToken
			seenToken = true

			switch character {
			case '/':
				if index+1 < len(line) {
					switch line[index+1] {
					case '/':
						return nil
					case '*':
						state.blockComment = true
						index++
						continue
					}
				}
				state.clearPendingControlContext()
				if state.canStartRegex || atLineStart {
					regex = true
					regexCharacterClass = false
					continue
				}
				state.canStartRegex = true

			case '\'', '"':
				state.clearPendingControlContext()
				state.stringQuote = character

			case '`':
				state.clearPendingControlContext()
				if state.templateInterpolation {
					state.uncertain = true
					return nil
				}
				state.templateRaw = true

			case '<':
				state.clearPendingControlContext()
				if state.canStartRegex && startsJavaScriptJSX(line, index) {
					if state.jsxExpression {
						state.uncertain = true
						return nil
					}
					state.startJSX(line, &index)
					continue
				}
				state.canStartRegex = true

			case '{':
				delimiter := javaScriptGenericBrace
				if state.pendingControlBlock {
					delimiter = javaScriptControlBlockBrace
				}
				state.clearPendingControlContext()
				if !state.pushDelimiter(delimiter) {
					return nil
				}
				state.canStartRegex = true

			case '}':
				state.clearPendingControlContext()
				delimiter, valid := state.popBrace()
				if !valid {
					return nil
				}
				switch delimiter {
				case javaScriptTemplateInterpolationBrace:
					state.templateInterpolation = false
					state.templateRaw = true
					state.canStartRegex = false
				case javaScriptJSXExpressionBrace:
					state.jsxExpression = false
					state.canStartRegex = false
				case javaScriptControlBlockBrace:
					state.canStartRegex = true
				default:
					state.canStartRegex = false
				}

			case '(':
				controlHeader := state.pendingControlParen
				state.clearPendingControlContext()
				delimiter := javaScriptExpressionParen
				if controlHeader {
					delimiter = javaScriptControlHeaderParen
				}
				if !state.pushDelimiter(delimiter) {
					return nil
				}
				state.canStartRegex = true

			case ')':
				state.clearPendingControlContext()
				paren, valid := state.popParen()
				if !valid {
					return nil
				}
				if paren == javaScriptControlHeaderParen {
					state.canStartRegex = true
					state.pendingControlBlock = true
				} else {
					state.canStartRegex = false
				}

			case '[':
				state.clearPendingControlContext()
				if !state.pushDelimiter(javaScriptBracket) {
					return nil
				}
				state.canStartRegex = true

			case ']':
				state.clearPendingControlContext()
				if !state.popExactDelimiter(javaScriptBracket) {
					return nil
				}
				state.canStartRegex = false

			case ',', ';', ':', '?', '=', '!', '~', '+', '-', '*', '%', '&', '|', '^', '>':
				state.clearPendingControlContext()
				state.canStartRegex = true

			case '.':
				state.clearPendingControlContext()
				state.canStartRegex = false

			default:
				if isJavaScriptIdentifierStart(character) {
					end := index + 1
					for end < len(line) && isJavaScriptIdentifierPart(line[end]) {
						if end%javaScriptCancellationStride == 0 {
							if err := ctx.Err(); err != nil {
								return err
							}
						}
						end++
					}
					token := line[index:end]
					state.pendingControlParen = javaScriptControlHeaderKeyword(token)
					state.pendingControlBlock = javaScriptControlBlockKeyword(token)
					state.canStartRegex = javaScriptKeywordAllowsExpression(token)
					index = end - 1
				} else if character >= '0' && character <= '9' {
					state.clearPendingControlContext()
					end := index + 1
					for end < len(line) && isJavaScriptNumberPart(line[end]) {
						if end%javaScriptCancellationStride == 0 {
							if err := ctx.Err(); err != nil {
								return err
							}
						}
						end++
					}
					state.canStartRegex = false
					index = end - 1
				} else {
					state.clearPendingControlContext()
					state.canStartRegex = true
				}
			}
		}
	}

	if regex || regexCharacterClass {
		state.uncertain = true
	}
	if state.stringQuote != 0 && !escaped {
		state.uncertain = true
	}
	return ctx.Err()
}

func (state *javaScriptLexicalState) clearPendingControlContext() {
	state.pendingControlParen = false
	state.pendingControlBlock = false
}

func (state *javaScriptLexicalState) pushDelimiter(delimiter javaScriptDelimiterKind) bool {
	if state.delimiterDepth == len(state.delimiters) {
		state.uncertain = true
		return false
	}
	state.delimiters[state.delimiterDepth] = delimiter
	state.delimiterDepth++
	return true
}

func (state *javaScriptLexicalState) popDelimiter() (javaScriptDelimiterKind, bool) {
	if state.delimiterDepth == 0 {
		state.uncertain = true
		return javaScriptExpressionParen, false
	}
	state.delimiterDepth--
	return state.delimiters[state.delimiterDepth], true
}

func (state *javaScriptLexicalState) popExactDelimiter(expected javaScriptDelimiterKind) bool {
	delimiter, valid := state.popDelimiter()
	if !valid || delimiter != expected {
		state.uncertain = true
		return false
	}
	return true
}

func (state *javaScriptLexicalState) popParen() (javaScriptDelimiterKind, bool) {
	delimiter, valid := state.popDelimiter()
	if !valid || delimiter != javaScriptExpressionParen && delimiter != javaScriptControlHeaderParen {
		state.uncertain = true
		return javaScriptExpressionParen, false
	}
	return delimiter, true
}

func (state *javaScriptLexicalState) popBrace() (javaScriptDelimiterKind, bool) {
	delimiter, valid := state.popDelimiter()
	if !valid || delimiter != javaScriptGenericBrace && delimiter != javaScriptControlBlockBrace &&
		delimiter != javaScriptTemplateInterpolationBrace && delimiter != javaScriptJSXExpressionBrace {
		state.uncertain = true
		return javaScriptGenericBrace, false
	}
	return delimiter, true
}

func startsJavaScriptJSX(line string, index int) bool {
	return index+1 < len(line) &&
		(isJavaScriptIdentifierStart(line[index+1]) || line[index+1] == '>')
}

func (state *javaScriptLexicalState) startJSX(line string, index *int) {
	state.jsxDepth++
	state.jsxClosingTag = false
	state.canStartRegex = false
	if line[*index+1] == '>' {
		state.jsxInTag = false
		(*index)++
		return
	}
	state.jsxInTag = true
}

func (state *javaScriptLexicalState) consumeJSXCharacter(line string, index *int) {
	character := line[*index]
	if state.jsxQuote != 0 {
		if state.jsxEscaped {
			state.jsxEscaped = false
			return
		}
		if character == '\\' {
			state.jsxEscaped = true
			return
		}
		if character == state.jsxQuote {
			state.jsxQuote = 0
		}
		return
	}

	if state.jsxInTag {
		switch character {
		case '\'', '"':
			state.jsxQuote = character
		case '{':
			state.startJSXExpression()
		case '/':
			if *index+1 < len(line) && line[*index+1] == '>' {
				state.jsxDepth--
				state.jsxInTag = false
				(*index)++
				state.finishJSXIfClosed()
			}
		case '>':
			if state.jsxClosingTag {
				state.jsxDepth--
			}
			state.jsxInTag = false
			state.jsxClosingTag = false
			state.finishJSXIfClosed()
		}
		return
	}

	switch character {
	case '{':
		state.startJSXExpression()
	case '<':
		if *index+1 >= len(line) {
			return
		}
		switch next := line[*index+1]; {
		case next == '/':
			state.jsxInTag = true
			state.jsxClosingTag = true
			(*index)++
		case next == '>':
			state.jsxDepth++
			(*index)++
		case isJavaScriptIdentifierStart(next):
			state.jsxDepth++
			state.jsxInTag = true
			state.jsxClosingTag = false
		}
	}
}

func (state *javaScriptLexicalState) startJSXExpression() {
	if !state.pushDelimiter(javaScriptJSXExpressionBrace) {
		return
	}
	state.jsxExpression = true
	state.canStartRegex = true
}

func (state *javaScriptLexicalState) finishJSXIfClosed() {
	if state.jsxDepth != 0 {
		return
	}
	state.jsxInTag = false
	state.jsxClosingTag = false
	state.jsxQuote = 0
	state.jsxEscaped = false
	state.canStartRegex = false
}

func isJavaScriptWhitespace(character byte) bool {
	return character == ' ' || character == '\t' || character == '\r' || character == '\n'
}

func isJavaScriptIdentifierStart(character byte) bool {
	return character == '$' || character == '_' || character >= 'A' && character <= 'Z' ||
		character >= 'a' && character <= 'z'
}

func isJavaScriptIdentifierPart(character byte) bool {
	return isJavaScriptIdentifierStart(character) || character >= '0' && character <= '9'
}

func isJavaScriptNumberPart(character byte) bool {
	return character == '.' || character == '_' || character >= '0' && character <= '9' ||
		character >= 'A' && character <= 'F' || character >= 'a' && character <= 'f' ||
		character == 'x' || character == 'X' || character == 'o' || character == 'O' ||
		character == 'b' || character == 'B' || character == 'e' || character == 'E' ||
		character == 'n'
}

func javaScriptKeywordAllowsExpression(keyword string) bool {
	switch keyword {
	case "await", "case", "delete", "do", "else", "in", "instanceof", "new", "of", "return", "throw", "typeof", "void", "yield":
		return true
	default:
		return false
	}
}

func javaScriptControlHeaderKeyword(keyword string) bool {
	switch keyword {
	case "catch", "for", "if", "switch", "while", "with":
		return true
	default:
		return false
	}
}

func javaScriptControlBlockKeyword(keyword string) bool {
	switch keyword {
	case "do", "else", "finally", "try":
		return true
	default:
		return false
	}
}
