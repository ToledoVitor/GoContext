// Package taintcheck provides bounded, encoding-aware detection for synthetic
// security proofs. Coverage is limited to raw bytes and the Base64, hex, and
// byte-debug encodings parsed here; it does not cover compression or encryption.
package taintcheck

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"strconv"
)

const (
	maxEncodedScanBytes  = 72 << 20
	maxEncodedTokenBytes = 1 << 20

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
	for offset := 0; offset < len(payload); {
		if !base64TokenByte(payload[offset]) {
			offset++
			continue
		}
		end := offset + 1
		for end < len(payload) && base64TokenByte(payload[end]) {
			end++
		}
		token := payload[offset:end]
		if len(token) > maxEncodedTokenBytes {
			return Match{}, false, false
		}
		for _, decoded := range decodeWholeBase64Token(token) {
			encoding := EncodingBase64
			if decoded.url {
				encoding = EncodingBase64URL
			}
			if match, found := findRaw(decoded.payload, canaries, encoding); found {
				return match, true, true
			}
		}
		offset = end
	}
	return Match{}, false, true
}

type decodedBase64 struct {
	payload []byte
	url     bool
}

func decodeWholeBase64Token(token []byte) []decodedBase64 {
	core, padding, valid := splitBase64Padding(token)
	if !valid || len(core) < 2 || len(core)%4 == 1 {
		return nil
	}
	requiredPadding := (4 - len(core)%4) % 4
	if requiredPadding == 3 || (padding != 0 && padding != requiredPadding) {
		return nil
	}
	padded := make([]byte, len(core)+requiredPadding)
	copy(padded, core)
	for index := len(core); index < len(padded); index++ {
		padded[index] = '='
	}

	variants := []struct {
		padded *base64.Encoding
		raw    *base64.Encoding
		url    bool
	}{
		{padded: base64.StdEncoding, raw: base64.RawStdEncoding},
		{padded: base64.URLEncoding, raw: base64.RawURLEncoding, url: true},
	}
	decoded := make([]decodedBase64, 0, len(variants)*2)
	for _, variant := range variants {
		for _, candidate := range []struct {
			encoding *base64.Encoding
			payload  []byte
		}{
			{encoding: variant.padded, payload: padded},
			{encoding: variant.raw, payload: core},
		} {
			buffer := make([]byte, candidate.encoding.DecodedLen(len(candidate.payload)))
			count, err := candidate.encoding.Decode(buffer, candidate.payload)
			if err != nil {
				continue
			}
			buffer = buffer[:count]
			duplicate := false
			for _, prior := range decoded {
				if prior.url == variant.url && bytes.Equal(prior.payload, buffer) {
					duplicate = true
					break
				}
			}
			if !duplicate {
				decoded = append(decoded, decodedBase64{payload: buffer, url: variant.url})
			}
		}
	}
	return decoded
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

func base64TokenByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '+' || value == '/' ||
		value == '-' || value == '_' || value == '='
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
	for offset := 0; offset < len(payload); offset++ {
		if payload[offset] != '[' {
			continue
		}
		endOffset := bytes.IndexByte(payload[offset+1:], ']')
		if endOffset < 0 {
			continue
		}
		end := offset + 1 + endOffset
		if end-offset+1 > maxEncodedTokenBytes {
			return Match{}, false, false
		}
		decoded, valid := parseDecimalBytes(payload[offset+1 : end])
		if valid {
			if match, found := findRaw(decoded, canaries, EncodingDecimalBytes); found {
				return match, true, true
			}
		}
		offset = end
	}
	return Match{}, false, true
}

func parseDecimalBytes(payload []byte) ([]byte, bool) {
	fields := bytes.Fields(payload)
	if len(fields) == 0 {
		return nil, false
	}
	decoded := make([]byte, len(fields))
	for index, field := range fields {
		value, err := strconv.ParseUint(string(field), 10, 8)
		if err != nil {
			return nil, false
		}
		decoded[index] = byte(value)
	}
	return decoded, true
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
