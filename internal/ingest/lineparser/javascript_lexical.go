package lineparser

import "context"

const javaScriptCancellationStride = 4 << 10

// javaScriptLexicalState tracks only the lexical context needed to decide
// whether a declaration starts at syntactic file scope. It is intentionally
// smaller than a JavaScript grammar and fails closed after unsupported nested
// template forms or malformed unterminated literals.
type javaScriptLexicalState struct {
	blockComment          bool
	blockDepth            int
	canStartRegex         bool
	stringQuote           byte
	templateRaw           bool
	templateInterpolation bool
	templateBraceDepth    int
	uncertain             bool
}

func newJavaScriptLexicalState() *javaScriptLexicalState {
	return &javaScriptLexicalState{canStartRegex: true}
}

func (state *javaScriptLexicalState) atTopLevel() bool {
	return !state.uncertain && !state.blockComment && state.stringQuote == 0 &&
		!state.templateRaw && !state.templateInterpolation && state.blockDepth == 0
}

func (state *javaScriptLexicalState) consume(ctx context.Context, line string) error {
	if state.uncertain {
		return ctx.Err()
	}

	escaped := false
	regex := false
	regexCharacterClass := false
	seenToken := false
	previousTokenWasLessThan := false
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
				state.templateRaw = false
				state.templateInterpolation = true
				state.templateBraceDepth = 1
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

		default:
			if isJavaScriptWhitespace(character) {
				continue
			}
			atLineStart := !seenToken
			seenToken = true
			afterLessThan := previousTokenWasLessThan
			previousTokenWasLessThan = character == '<'

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
				if afterLessThan && index+1 < len(line) &&
					(isJavaScriptIdentifierStart(line[index+1]) || line[index+1] == '>') {
					state.canStartRegex = true
					continue
				}
				if state.canStartRegex || atLineStart {
					regex = true
					regexCharacterClass = false
					continue
				}
				state.canStartRegex = true

			case '\'', '"':
				state.stringQuote = character

			case '`':
				if state.templateInterpolation {
					state.uncertain = true
					return nil
				}
				state.templateRaw = true

			case '{':
				if state.templateInterpolation {
					state.templateBraceDepth++
				} else {
					state.blockDepth++
				}
				state.canStartRegex = true

			case '}':
				if state.templateInterpolation {
					state.templateBraceDepth--
					if state.templateBraceDepth == 0 {
						state.templateInterpolation = false
						state.templateRaw = true
					}
				} else if state.blockDepth > 0 {
					state.blockDepth--
				} else {
					state.uncertain = true
					return nil
				}
				state.canStartRegex = false

			case ')', ']':
				state.canStartRegex = false

			case '(', '[', ',', ';', ':', '?', '=', '!', '~', '+', '-', '*', '%', '&', '|', '^', '<', '>':
				state.canStartRegex = true

			case '.':
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
					state.canStartRegex = javaScriptKeywordAllowsExpression(line[index:end])
					index = end - 1
				} else if character >= '0' && character <= '9' {
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
