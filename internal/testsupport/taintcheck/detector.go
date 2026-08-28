// Package taintcheck provides bounded, encoding-aware detection for synthetic
// security proofs. Coverage is limited to raw bytes and the Base64, hex, and
// byte-debug encodings parsed here; it does not cover compression or encryption.
package taintcheck

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
)

const (
	maxEncodedScanBytes       = 72 << 20
	maxEncodedTokenBytes      = 1 << 20
	maxEncodedTokenCount      = 1 << 20
	maxTotalEncodedTokenBytes = maxEncodedScanBytes
	maxBase64ScanSteps        = 2 * maxEncodedScanBytes

	EncodingRaw          = "raw"
	EncodingBase64       = "base64"
	EncodingBase64URL    = "base64url"
	EncodingHex          = "hex"
	EncodingEscapedBytes = "escaped-bytes"
	EncodingGoDebugBytes = "go-debug-bytes"
	EncodingDecimalBytes = "decimal-bytes"
)

// Match identifies the caller-supplied canary and representation found.
type Match struct {
	Canary   string
	Encoding string
}

// Result reports a match and whether every supported representation was
// inspected within the package's explicit payload and token work bounds.
type Result struct {
	Match    Match
	Found    bool
	Complete bool
}

// Scan searches payload for canaries directly and by fully decoding maximal
// plausible tokens in every supported representation.
func Scan(payload []byte, canaries []string) Result {
	if len(payload) > maxEncodedScanBytes {
		return Result{}
	}
	normalized := nonEmptyCanaries(canaries)
	result := Result{Complete: true}
	if match, found := findRaw(payload, normalized, EncodingRaw); found {
		result.Match, result.Found = match, true
		return result
	}

	finders := []func([]byte, [][]byte) (Match, bool, bool){
		findBase64Tokens,
		findHexTokens,
		findEscapedByteTokens,
		findGoDebugByteTokens,
		findDecimalByteTokens,
	}
	for _, finder := range finders {
		match, found, complete := finder(payload, normalized)
		if !complete {
			return Result{}
		}
		if found {
			result.Match, result.Found = match, true
			return result
		}
	}
	return result
}

func nonEmptyCanaries(canaries []string) [][]byte {
	normalized := make([][]byte, 0, len(canaries))
	for _, canary := range canaries {
		if canary != "" {
			normalized = append(normalized, []byte(canary))
		}
	}
	return normalized
}

func findRaw(payload []byte, canaries [][]byte, encoding string) (Match, bool) {
	for _, canary := range canaries {
		if bytes.Contains(payload, canary) {
			return Match{Canary: string(canary), Encoding: encoding}, true
		}
	}
	return Match{}, false
}

func findBase64Tokens(payload []byte, canaries [][]byte) (Match, bool, bool) {
	match, found, complete, _ := findBase64TokensWithLimits(payload, canaries, base64ScanLimits{
		maxTokenBytes:      maxEncodedTokenBytes,
		maxTokenCount:      maxEncodedTokenCount,
		maxTotalTokenBytes: maxTotalEncodedTokenBytes,
		maxSteps:           maxBase64ScanSteps,
	})
	return match, found, complete
}

type base64ScanLimits struct {
	maxTokenBytes      int
	maxTokenCount      int
	maxTotalTokenBytes int
	maxSteps           int
}

// findBase64TokensWithLimits charges both lexical passes to one ledger. Exact
// token spans common to both alphabets are decoded and charged only once.
func findBase64TokensWithLimits(
	payload []byte,
	canaries [][]byte,
	limits base64ScanLimits,
) (Match, bool, bool, base64ScanWork) {
	work := base64ScanWork{
		limits: limits,
		seen:   make(map[base64TokenSpan]struct{}),
	}
	passes := []base64LexicalPass{
		{alphabet: standardBase64AlphabetByte, encoding: base64.StdEncoding, label: EncodingBase64},
		{alphabet: urlBase64AlphabetByte, encoding: base64.URLEncoding, label: EncodingBase64URL},
	}
	for _, pass := range passes {
		match, found, complete := findBase64TokensForPass(payload, canaries, pass, &work)
		if !complete || found {
			return match, found, complete, work
		}
	}
	return Match{}, false, true, work
}

type base64LexicalPass struct {
	alphabet func(byte) bool
	encoding *base64.Encoding
	label    string
}

type base64ScanWork struct {
	tokenCount      int
	totalTokenBytes int
	steps           int
	limits          base64ScanLimits
	seen            map[base64TokenSpan]struct{}
}

type base64TokenSpan struct {
	start int
	end   int
}

func findBase64TokensForPass(
	payload []byte,
	canaries [][]byte,
	pass base64LexicalPass,
	work *base64ScanWork,
) (Match, bool, bool) {
	for offset := 0; offset < len(payload); {
		if !work.takeStep() {
			return Match{}, false, false
		}
		if !pass.alphabet(payload[offset]) {
			offset++
			continue
		}
		start := offset
		offset++
		for offset < len(payload) && pass.alphabet(payload[offset]) {
			if !work.takeStep() {
				return Match{}, false, false
			}
			offset++
		}
		end := base64PaddedTokenEnd(payload, start, offset, pass.alphabet)
		duplicate, withinLimits := work.takeToken(start, end)
		if !withinLimits {
			return Match{}, false, false
		}
		if duplicate {
			continue
		}
		decoded, valid := decodeWholeBase64Token(payload[start:end], pass.encoding)
		if valid {
			if match, found := findRaw(decoded, canaries, pass.label); found {
				return match, true, true
			}
		}
	}
	return Match{}, false, true
}

func (work *base64ScanWork) takeStep() bool {
	if work.steps >= work.limits.maxSteps {
		return false
	}
	work.steps++
	return true
}

func (work *base64ScanWork) takeToken(start, end int) (bool, bool) {
	length := end - start
	if length > work.limits.maxTokenBytes {
		return false, false
	}
	span := base64TokenSpan{start: start, end: end}
	if _, present := work.seen[span]; present {
		return true, true
	}
	work.seen[span] = struct{}{}
	work.tokenCount++
	work.totalTokenBytes += length
	if work.tokenCount > work.limits.maxTokenCount ||
		work.totalTokenBytes > work.limits.maxTotalTokenBytes {
		return false, false
	}
	return false, true
}

func base64PaddedTokenEnd(payload []byte, start, coreEnd int, alphabet func(byte) bool) int {
	requiredPadding := (4 - (coreEnd-start)%4) % 4
	if requiredPadding < 1 || requiredPadding > 2 || coreEnd+requiredPadding > len(payload) {
		return coreEnd
	}
	for index := coreEnd; index < coreEnd+requiredPadding; index++ {
		if payload[index] != '=' {
			return coreEnd
		}
	}
	paddedEnd := coreEnd + requiredPadding
	if paddedEnd < len(payload) && (alphabet(payload[paddedEnd]) || payload[paddedEnd] == '=') {
		return coreEnd
	}
	return paddedEnd
}

func decodeWholeBase64Token(token []byte, encoding *base64.Encoding) ([]byte, bool) {
	core, padding, valid := splitBase64Padding(token)
	if !valid || len(core) < 2 || len(core)%4 == 1 {
		return nil, false
	}
	requiredPadding := (4 - len(core)%4) % 4
	if requiredPadding == 3 || (padding != 0 && padding != requiredPadding) {
		return nil, false
	}
	padded := make([]byte, len(core)+requiredPadding)
	copy(padded, core)
	for index := len(core); index < len(padded); index++ {
		padded[index] = '='
	}
	buffer := make([]byte, encoding.DecodedLen(len(padded)))
	count, err := encoding.Decode(buffer, padded)
	if err != nil {
		return nil, false
	}
	return buffer[:count], true
}

func splitBase64Padding(token []byte) ([]byte, int, bool) {
	first := bytes.IndexByte(token, '=')
	if first < 0 {
		return token, 0, true
	}
	padding := len(token) - first
	if padding > 2 {
		return nil, 0, false
	}
	for _, value := range token[first:] {
		if value != '=' {
			return nil, 0, false
		}
	}
	return token[:first], padding, true
}

func standardBase64AlphabetByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '+' || value == '/'
}

func urlBase64AlphabetByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '-' || value == '_'
}

func findHexTokens(payload []byte, canaries [][]byte) (Match, bool, bool) {
	for offset := 0; offset < len(payload); {
		if !hexByte(payload[offset]) {
			offset++
			continue
		}
		end := offset + 1
		for end < len(payload) && hexByte(payload[end]) {
			end++
		}
		token := payload[offset:end]
		if len(token) > maxEncodedTokenBytes {
			return Match{}, false, false
		}
		if len(token) >= 2 && len(token)%2 == 0 {
			decoded := make([]byte, hex.DecodedLen(len(token)))
			if _, err := hex.Decode(decoded, token); err == nil {
				if match, found := findRaw(decoded, canaries, EncodingHex); found {
					return match, true, true
				}
			}
		}
		offset = end
	}
	return Match{}, false, true
}

func findEscapedByteTokens(payload []byte, canaries [][]byte) (Match, bool, bool) {
	for offset := 0; offset+3 < len(payload); {
		if payload[offset] != '\\' || payload[offset+1] != 'x' ||
			!hexByte(payload[offset+2]) || !hexByte(payload[offset+3]) {
			offset++
			continue
		}
		end := offset
		decoded := make([]byte, 0)
		for end+3 < len(payload) && payload[end] == '\\' && payload[end+1] == 'x' &&
			hexByte(payload[end+2]) && hexByte(payload[end+3]) {
			if end-offset+4 > maxEncodedTokenBytes {
				return Match{}, false, false
			}
			decoded = append(decoded, fromHexPair(payload[end+2], payload[end+3]))
			end += 4
		}
		if match, found := findRaw(decoded, canaries, EncodingEscapedBytes); found {
			return match, true, true
		}
		offset = end
	}
	return Match{}, false, true
}

func findGoDebugByteTokens(payload []byte, canaries [][]byte) (Match, bool, bool) {
	for offset := 0; offset+3 < len(payload); {
		value, next, valid := parseGoHexByte(payload, offset)
		if !valid {
			offset++
			continue
		}
		decoded := []byte{value}
		end := next
		for {
			separator := end
			for separator < len(payload) && (payload[separator] == ' ' || payload[separator] == '\t') {
				separator++
			}
			if separator >= len(payload) || payload[separator] != ',' {
				break
			}
			separator++
			for separator < len(payload) && (payload[separator] == ' ' || payload[separator] == '\t') {
				separator++
			}
			value, next, valid = parseGoHexByte(payload, separator)
			if !valid {
				break
			}
			if next-offset > maxEncodedTokenBytes {
				return Match{}, false, false
			}
			decoded = append(decoded, value)
			end = next
		}
		if match, found := findRaw(decoded, canaries, EncodingGoDebugBytes); found {
			return match, true, true
		}
		offset = end
	}
	return Match{}, false, true
}

func parseGoHexByte(payload []byte, offset int) (byte, int, bool) {
	if offset+3 >= len(payload) || payload[offset] != '0' || payload[offset+1] != 'x' ||
		!hexByte(payload[offset+2]) || !hexByte(payload[offset+3]) ||
		offset+4 < len(payload) && hexByte(payload[offset+4]) {
		return 0, offset, false
	}
	return fromHexPair(payload[offset+2], payload[offset+3]), offset + 4, true
}

func findDecimalByteTokens(payload []byte, canaries [][]byte) (Match, bool, bool) {
	match, found, complete, _ := findDecimalByteTokensWithBudget(payload, canaries, len(payload))
	return match, found, complete
}

// findDecimalByteTokensWithBudget charges exactly one step for each input byte
// visited so tests can prove malformed brackets cannot trigger suffix rescans.
func findDecimalByteTokensWithBudget(
	payload []byte,
	canaries [][]byte,
	stepBudget int,
) (Match, bool, bool, int) {
	const (
		decimalBeforeValue = iota
		decimalInValue
		decimalBetweenValues
	)
	active := false
	start := 0
	state := decimalBeforeValue
	value := 0
	digits := 0
	decoded := make([]byte, 0)
	steps := 0
	for offset, current := range payload {
		if steps >= stepBudget {
			return Match{}, false, false, steps
		}
		steps++
		if current == '[' {
			active = true
			start = offset
			state = decimalBeforeValue
			value = 0
			digits = 0
			decoded = decoded[:0]
			continue
		}
		if !active {
			continue
		}
		if offset-start+1 > maxEncodedTokenBytes {
			return Match{}, false, false, steps
		}

		switch state {
		case decimalBeforeValue:
			switch {
			case decimalWhitespace(current):
			case decimalDigit(current):
				state = decimalInValue
				value = int(current - '0')
				digits = 1
			default:
				active = false
			}
		case decimalInValue:
			switch {
			case decimalDigit(current):
				value = value*10 + int(current-'0')
				digits++
				if digits > 3 || value > 255 {
					active = false
				}
			case decimalWhitespace(current):
				decoded = append(decoded, byte(value))
				state = decimalBetweenValues
			case current == ']':
				decoded = append(decoded, byte(value))
				if match, found := findRaw(decoded, canaries, EncodingDecimalBytes); found {
					return match, true, true, steps
				}
				active = false
			default:
				active = false
			}
		case decimalBetweenValues:
			switch {
			case decimalWhitespace(current):
			case decimalDigit(current):
				state = decimalInValue
				value = int(current - '0')
				digits = 1
			case current == ']':
				if match, found := findRaw(decoded, canaries, EncodingDecimalBytes); found {
					return match, true, true, steps
				}
				active = false
			default:
				active = false
			}
		}
	}
	return Match{}, false, true, steps
}

func decimalDigit(value byte) bool {
	return value >= '0' && value <= '9'
}

func decimalWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r' || value == '\v' || value == '\f'
}

func hexByte(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func fromHexPair(high, low byte) byte {
	return hexNibble(high)<<4 | hexNibble(low)
}

func hexNibble(value byte) byte {
	switch {
	case value >= '0' && value <= '9':
		return value - '0'
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10
	default:
		return value - 'A' + 10
	}
}
