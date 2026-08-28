package taintcheck

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func TestScanDetectsShortCanaryInsideWholeBase64TokensAtEveryAlignment(t *testing.T) {
	const canary = ".env"
	encodings := []struct {
		name   string
		encode func([]byte) string
	}{
		{name: "standard padded", encode: base64.StdEncoding.EncodeToString},
		{name: "standard raw", encode: base64.RawStdEncoding.EncodeToString},
		{name: "URL padded", encode: base64.URLEncoding.EncodeToString},
		{name: "URL raw", encode: base64.RawURLEncoding.EncodeToString},
	}

	for prefixLength := 0; prefixLength < 3; prefixLength++ {
		prefix := []byte{0xfb, 0xef, 0xff}[:prefixLength:prefixLength]
		payload := append(append([]byte(nil), prefix...), []byte(canary)...)
		payload = append(payload, 0xfa, 0x00, 0x7f)
		near := append(append([]byte(nil), prefix...), []byte(".enw")...)
		near = append(near, 0xfa, 0x00, 0x7f)
		for _, encoding := range encodings {
			t.Run(fmt.Sprintf("%s/prefix-%d", encoding.name, prefixLength), func(t *testing.T) {
				wrapped := []byte("larger-wrapper:" + encoding.encode(payload) + ":suffix")
				result := Scan(wrapped, []string{canary})
				if !result.Complete || !result.Found || result.Match.Canary != canary {
					t.Fatalf("Scan() = %#v; want complete encoded-canary match", result)
				}
				nearResult := Scan([]byte("larger-wrapper:"+encoding.encode(near)+":suffix"), []string{canary})
				if !nearResult.Complete || nearResult.Found {
					t.Fatalf("Scan(same-length near value) = %#v; want complete no-match", nearResult)
				}
			})
		}
	}
}

func TestScanDetectsContextIndependentHexAndEscapedDebugSubsequences(t *testing.T) {
	const canary = "SYNTHETIC_DEBUG_DETECTOR_CANARY_TASK13"
	prefixed := append([]byte("prefix"), []byte(canary)...)
	prefixed = append(prefixed, []byte("suffix")...)

	escaped := make([]string, 0, len(prefixed))
	for _, value := range prefixed {
		escaped = append(escaped, fmt.Sprintf(`\x%02x`, value))
	}
	variants := map[string][]byte{
		"lower hex":     []byte("wrapper:" + hex.EncodeToString(prefixed) + ":suffix"),
		"upper hex":     []byte("wrapper:" + strings.ToUpper(hex.EncodeToString(prefixed)) + ":suffix"),
		"escaped bytes": []byte("wrapper:" + strings.Join(escaped, "") + ":suffix"),
		"Go debug":      []byte(fmt.Sprintf("wrapper:%#v:suffix", prefixed)),
		"decimal debug": []byte(fmt.Sprintf("wrapper:%v:suffix", prefixed)),
	}
	for name, payload := range variants {
		t.Run(name, func(t *testing.T) {
			result := Scan(payload, []string{canary})
			if !result.Complete || !result.Found || result.Match.Canary != canary {
				t.Fatalf("Scan() = %#v; want %s canary", result, name)
			}
		})
	}
}

func TestScanRejectsNearMatchAndReportsRawCanary(t *testing.T) {
	const canary = "SYNTHETIC_NEGATIVE_DETECTOR_CANARY_TASK13"
	near := Scan([]byte("SYNTHETIC_NEGATIVE_DETECTOR_CANARY_TASK14"), []string{canary})
	if !near.Complete || near.Found {
		t.Fatalf("Scan(near match) = %#v; want complete no-match", near)
	}
	result := Scan([]byte("prefix:"+canary+":suffix"), []string{canary})
	if !result.Complete || !result.Found || result.Match.Canary != canary || result.Match.Encoding != EncodingRaw {
		t.Fatalf("Scan(raw) = %#v; want complete raw canary", result)
	}
}

func TestScanReportsIncompleteInsteadOfSkippingOversizedEncodedToken(t *testing.T) {
	payload := append([]byte("wrapper:"), bytes.Repeat([]byte{'A'}, maxEncodedTokenBytes+1)...)
	result := Scan(payload, []string{".env"})
	if result.Complete || result.Found {
		t.Fatalf("Scan(oversized token) = %#v, want incomplete no-match", result)
	}
}
