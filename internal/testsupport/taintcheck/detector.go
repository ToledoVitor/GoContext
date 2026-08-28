// Package taintcheck provides encoding-aware byte detection for synthetic
// security proofs. It intentionally does not claim to detect compression or
// encryption; callers must also inspect structured values at semantic sinks.
package taintcheck

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	EncodingRaw          = "raw"
	EncodingBase64       = "base64"
	EncodingBase64URL    = "base64url"
	EncodingHex          = "hex"
	EncodingEscapedBytes = "escaped-bytes"
	EncodingGoDebugBytes = "go-debug-bytes"
	EncodingDecimalBytes = "decimal-bytes"
)

// Match identifies the canary and representation found in a payload.
type Match struct {
	Canary   string
	Encoding string
}

type variant struct {
	encoding string
	payload  []byte
}

// Find reports whether payload contains any canary directly or in a supported
// context-independent representation.
func Find(payload []byte, canaries []string) (Match, bool) {
	for _, canary := range canaries {
		if canary == "" {
			continue
		}
		if bytes.Contains(payload, []byte(canary)) {
			return Match{Canary: canary, Encoding: EncodingRaw}, true
		}
		for _, candidate := range encodedVariants([]byte(canary)) {
			if len(candidate.payload) != 0 && bytes.Contains(payload, candidate.payload) {
				return Match{Canary: canary, Encoding: candidate.encoding}, true
			}
		}
	}
	return Match{}, false
}

func encodedVariants(canary []byte) []variant {
	variants := []variant{
		{encoding: EncodingBase64, payload: []byte(base64.StdEncoding.EncodeToString(canary))},
		{encoding: EncodingBase64, payload: []byte(base64.RawStdEncoding.EncodeToString(canary))},
		{encoding: EncodingBase64URL, payload: []byte(base64.URLEncoding.EncodeToString(canary))},
		{encoding: EncodingBase64URL, payload: []byte(base64.RawURLEncoding.EncodeToString(canary))},
		{encoding: EncodingHex, payload: []byte(hex.EncodeToString(canary))},
		{encoding: EncodingHex, payload: []byte(strings.ToUpper(hex.EncodeToString(canary)))},
		{encoding: EncodingEscapedBytes, payload: escapedBytes(canary)},
		{encoding: EncodingGoDebugBytes, payload: debugByteInterior(canary, "%#v")},
		{encoding: EncodingDecimalBytes, payload: debugByteInterior(canary, "%v")},
	}

	// Base64 groups bytes in threes. For every possible number of preceding
	// bytes modulo three, select the longest canary interior that starts and
	// ends on a group boundary. Its encoding is therefore unchanged inside a
	// larger, unknown payload.
	for precedingResidue := 0; precedingResidue < 3; precedingResidue++ {
		skip := (3 - precedingResidue) % 3
		if skip >= len(canary) {
			continue
		}
		length := len(canary) - skip
		length -= length % 3
		if length < 3 {
			continue
		}
		interior := canary[skip : skip+length]
		variants = append(variants,
			variant{encoding: EncodingBase64, payload: []byte(base64.RawStdEncoding.EncodeToString(interior))},
			variant{encoding: EncodingBase64URL, payload: []byte(base64.RawURLEncoding.EncodeToString(interior))},
		)
	}
	return uniqueVariants(variants)
}

func escapedBytes(payload []byte) []byte {
	var escaped strings.Builder
	escaped.Grow(len(payload) * 4)
	for _, value := range payload {
		fmt.Fprintf(&escaped, `\x%02x`, value)
	}
	return []byte(escaped.String())
}

func debugByteInterior(payload []byte, format string) []byte {
	debug := fmt.Sprintf(format, payload)
	start := strings.IndexByte(debug, '{')
	end := strings.LastIndexByte(debug, '}')
	if start < 0 || end <= start {
		start = strings.IndexByte(debug, '[')
		end = strings.LastIndexByte(debug, ']')
	}
	if start < 0 || end <= start {
		return nil
	}
	return []byte(debug[start+1 : end])
}

func uniqueVariants(candidates []variant) []variant {
	seen := make(map[string]struct{}, len(candidates))
	unique := make([]variant, 0, len(candidates))
	for _, candidate := range candidates {
		key := candidate.encoding + "\x00" + string(candidate.payload)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, candidate)
	}
	return unique
}
