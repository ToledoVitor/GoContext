package taintcheck

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func TestFindDetectsCanaryInsideLargerEncodedPayloadsAtEveryAlignment(t *testing.T) {
	const canary = "SYNTHETIC_TAINT_DETECTOR_CANARY_TASK13"
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
		payload := append([]byte{0xfb, 0xef, 0xff}[:prefixLength:prefixLength], []byte(canary)...)
		payload = append(payload, 0xfa, 0x00, 0x7f)
		for _, encoding := range encodings {
			t.Run(fmt.Sprintf("%s/prefix-%d", encoding.name, prefixLength), func(t *testing.T) {
				wrapped := []byte("larger-wrapper:" + encoding.encode(payload) + ":suffix")
				match, found := Find(wrapped, []string{canary})
				if !found || match.Canary != canary {
					t.Fatalf("Find() = %#v, %t; want encoded canary", match, found)
				}
			})
		}
	}
}

func TestFindDetectsContextIndependentHexAndEscapedDebugSubsequences(t *testing.T) {
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
			match, found := Find(payload, []string{canary})
			if !found || match.Canary != canary {
				t.Fatalf("Find() = %#v, %t; want %s canary", match, found, name)
			}
		})
	}
}

func TestFindRejectsNearMatchAndReportsRawCanary(t *testing.T) {
	const canary = "SYNTHETIC_NEGATIVE_DETECTOR_CANARY_TASK13"
	if match, found := Find([]byte("SYNTHETIC_NEGATIVE_DETECTOR_CANARY_TASK14"), []string{canary}); found {
		t.Fatalf("Find(near match) = %#v, true; want no match", match)
	}
	match, found := Find([]byte("prefix:"+canary+":suffix"), []string{canary})
	if !found || match.Canary != canary || match.Encoding != EncodingRaw {
		t.Fatalf("Find(raw) = %#v, %t; want raw canary", match, found)
	}
}
