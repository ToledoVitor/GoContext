package taintcheck

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func TestScanDetectsShortCanaryAcrossDelimitedBase64TokensAtEveryAlignment(t *testing.T) {
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
	wrappers := []struct {
		name string
		wrap func(string) string
	}{
		{name: "assignment", wrap: func(encoded string) string { return "key=" + encoded }},
		{name: "colon", wrap: func(encoded string) string { return "value:" + encoded + ";" }},
		{name: "single quote", wrap: func(encoded string) string { return "value='" + encoded + "'" }},
		{name: "double quote", wrap: func(encoded string) string { return `value="` + encoded + `"` }},
		{name: "query", wrap: func(encoded string) string { return "?data=" + encoded + "&mode=safe" }},
		{name: "invalid leading alphabet token", wrap: func(encoded string) string { return "A=" + encoded }},
		{name: "invalid padding prefix", wrap: func(encoded string) string { return "value====" + encoded + ";" }},
	}

	for prefixLength := 0; prefixLength < 3; prefixLength++ {
		prefix := []byte{0xfb, 0xef, 0xff}[:prefixLength:prefixLength]
		payload := append(append([]byte(nil), prefix...), []byte(canary)...)
		payload = append(payload, 0xfa, 0x00, 0x7f)
		near := append(append([]byte(nil), prefix...), []byte(".enw")...)
		near = append(near, 0xfa, 0x00, 0x7f)
		for _, encoding := range encodings {
			for _, wrapper := range wrappers {
				t.Run(fmt.Sprintf("%s/prefix-%d/%s", encoding.name, prefixLength, wrapper.name), func(t *testing.T) {
					result := Scan([]byte(wrapper.wrap(encoding.encode(payload))), []string{canary})
					if !result.Complete || !result.Found || result.Match.Canary != canary {
						t.Fatalf("Scan() = %#v; want complete encoded-canary match", result)
					}
					nearResult := Scan([]byte(wrapper.wrap(encoding.encode(near))), []string{canary})
					if !nearResult.Complete || nearResult.Found {
						t.Fatalf("Scan(same-length near value) = %#v; want complete no-match", nearResult)
					}
				})
			}
		}
	}
}

func TestScanDetectsExactShortCanaryAfterBase64AssignmentDelimiter(t *testing.T) {
	const canary = ".env"
	for name, payload := range map[string]string{
		"standard padded":     "key=LmVudg==",
		"standard raw":        "key=LmVudg",
		"URL padded":          `encoded="LmVudg=="`,
		"URL raw":             `encoded='LmVudg'`,
		"query padded":        "?data=LmVudg==&mode=safe",
		"invalid leading run": "A=LmVudg==",
		"invalid padding run": "====LmVudg==",
	} {
		t.Run(name, func(t *testing.T) {
			result := Scan([]byte(payload), []string{canary})
			if !result.Complete || !result.Found || result.Match.Canary != canary {
				t.Fatalf("Scan(%q) = %#v; want complete encoded-canary match", payload, result)
			}
		})
	}

	for _, payload := range []string{"key=LmVudw==", "key=LmVudw"} {
		result := Scan([]byte(payload), []string{canary})
		if !result.Complete || result.Found {
			t.Fatalf("Scan(near %q) = %#v; want complete no-match", payload, result)
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

func TestScanDetectsDecimalDebugCanaryInLargerWrapperAndRecoversMalformedPrefix(t *testing.T) {
	const canary = "SYNTHETIC_DECIMAL_DETECTOR_CANARY_TASK13"
	prefixed := append([]byte("prefix"), []byte(canary)...)
	prefixed = append(prefixed, []byte("suffix")...)
	decimal := fmt.Sprintf("%v", prefixed)
	variants := map[string]string{
		"larger byte slice wrapper": "prefix=[]byte(" + decimal + ");suffix",
		"malformed word prefix":     "prefix=[not-a-byte " + decimal + ";suffix",
		"out-of-range prefix":       "prefix=[999 " + decimal + ";suffix",
	}
	for name, payload := range variants {
		t.Run(name, func(t *testing.T) {
			result := Scan([]byte(payload), []string{canary})
			if !result.Complete || !result.Found || result.Match.Canary != canary ||
				result.Match.Encoding != EncodingDecimalBytes {
				t.Fatalf("Scan() = %#v; want complete decimal canary match", result)
			}
		})
	}
}

func TestDecimalDebugScanHasDeterministicLinearStepBudget(t *testing.T) {
	payload := bytes.Repeat([]byte{'['}, 4<<20)
	_, found, complete, steps := findDecimalByteTokensWithBudget(
		payload, [][]byte{[]byte(".env")}, len(payload),
	)
	if !complete || found || steps != len(payload) {
		t.Fatalf("full scan = found %t complete %t steps %d; want complete no-match in %d steps", found, complete, steps, len(payload))
	}

	_, found, complete, steps = findDecimalByteTokensWithBudget(
		payload, [][]byte{[]byte(".env")}, len(payload)-1,
	)
	if complete || found || steps != len(payload)-1 {
		t.Fatalf("bounded scan = found %t complete %t steps %d; want incomplete at exact budget %d", found, complete, steps, len(payload)-1)
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
