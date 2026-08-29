package eval_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	evaluation "github.com/ToledoVitor/GoContext/internal/eval"
	"github.com/ToledoVitor/GoContext/internal/ingest"
)

func TestParseGoldSetAcceptsStrictSyntheticSchema(t *testing.T) {
	goldSet, err := evaluation.ParseGoldSet(context.Background(), validGoldSetPayload(), "repo-ab", ingest.ScanPolicyVersion)
	if err != nil || goldSet == nil {
		t.Fatalf("ParseGoldSet() = %#v, %v", goldSet, err)
	}
}

func TestParseGoldSetRejectsInvalidInputsWithSanitizedSentinel(t *testing.T) {
	canary := "private-query-path-CANARY"
	tests := []struct {
		name    string
		payload []byte
		repo    string
		policy  string
	}{
		{name: "empty cases", payload: replaceGold(validGoldSetPayload(), []byte(`"cases":[{`), []byte(`"cases":[],"removed":[{`)), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "query too long", payload: replaceGold(validGoldSetPayload(), []byte(`"private query"`), []byte(`"`+strings.Repeat("q", 4097)+`"`)), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "missing judgment", payload: replaceGold(validGoldSetPayload(), []byte(`"judgments":[{"reference":{"path":"permitted/relative.go","start_line":1,"end_line":10},"relevance":3}]`), []byte(`"judgments":[]`)), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "bad relevance", payload: replaceGold(validGoldSetPayload(), []byte(`"relevance":3`), []byte(`"relevance":4`)), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "unknown category", payload: replaceGold(validGoldSetPayload(), []byte(`"concept"`), []byte(`"private-category"`)), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "repository mismatch", payload: validGoldSetPayload(), repo: "repo-cd", policy: ingest.ScanPolicyVersion},
		{name: "policy mismatch", payload: validGoldSetPayload(), repo: "repo-ab", policy: "scanner-old"},
		{name: "unknown key", payload: replaceGold(validGoldSetPayload(), []byte(`"schema":1`), []byte(`"schema":1,"private":"`+canary+`"`)), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "missing key", payload: replaceGold(validGoldSetPayload(), []byte(`"id":"case-ab",`), nil), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "duplicate key", payload: replaceGold(validGoldSetPayload(), []byte(`"id":"case-ab"`), []byte(`"id":"case-ab","id":"`+canary+`"`)), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "trailing json", payload: append(validGoldSetPayload(), []byte(` {}`)...), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "control byte", payload: replaceGold(validGoldSetPayload(), []byte(`private query`), []byte(`private\u0000query`)), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "invalid utf8", payload: append(validGoldSetPayload(), 0xff), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "oversize", payload: bytes.Repeat([]byte("x"), int(evaluation.MaxGoldSetBytes)+1), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			goldSet, err := evaluation.ParseGoldSet(context.Background(), test.payload, test.repo, test.policy)
			if goldSet != nil || !errors.Is(err, evaluation.ErrGoldSet) || strings.Contains(err.Error(), canary) {
				t.Fatalf("ParseGoldSet() = %#v, %v", goldSet, err)
			}
		})
	}
}

func TestParseGoldSetRejectsDuplicatePrivateCaseFacts(t *testing.T) {
	base := string(validGoldSetPayload())
	caseBody := base[strings.Index(base, `{"id"`):strings.LastIndex(base, `]}`)]
	tests := []struct {
		name   string
		second string
	}{
		{name: "id", second: strings.Replace(caseBody, `"query":"private query"`, `"query":"other query"`, 1)},
		{name: "category query", second: strings.Replace(caseBody, `"id":"case-ab"`, `"id":"case-cd"`, 1)},
		{name: "reference", second: strings.Replace(caseBody, `"judgments":[`, `"judgments":[{"reference":{"path":"permitted/relative.go","start_line":1,"end_line":10},"relevance":2},`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(strings.Replace(base, caseBody, caseBody+","+test.second, 1))
			if _, err := evaluation.ParseGoldSet(context.Background(), payload, "repo-ab", ingest.ScanPolicyVersion); !errors.Is(err, evaluation.ErrGoldSet) {
				t.Fatalf("ParseGoldSet() error = %v", err)
			}
		})
	}
}

func TestParseGoldSetEnforcesNegativeEvidenceShapeAndCancellation(t *testing.T) {
	negative := []byte(`{"schema":1,"repository":"repo-ab","scan_policy_version":"scanner-v6","cases":[{"id":"case-no","category":"negative_evidence","query":"absent concept","expectation":"no_evidence","judgments":[]}]}`)
	if value, err := evaluation.ParseGoldSet(context.Background(), negative, "repo-ab", ingest.ScanPolicyVersion); err != nil || value == nil {
		t.Fatalf("negative ParseGoldSet() = %#v, %v", value, err)
	}
	for _, payload := range [][]byte{
		replaceGold(negative, []byte(`"negative_evidence"`), []byte(`"concept"`)),
		replaceGold(negative, []byte(`"judgments":[]`), []byte(`"judgments":[{"reference":{"path":"safe.go","start_line":1,"end_line":1},"relevance":1}]`)),
		replaceGold(negative, []byte(`,"judgments":[]`), nil),
		replaceGold(negative, []byte(`"judgments":[]`), []byte(`"judgments":null`)),
	} {
		if _, err := evaluation.ParseGoldSet(context.Background(), payload, "repo-ab", ingest.ScanPolicyVersion); !errors.Is(err, evaluation.ErrGoldSet) {
			t.Fatalf("negative shape error = %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := evaluation.ParseGoldSet(ctx, validGoldSetPayload(), "repo-ab", ingest.ScanPolicyVersion); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
}

func validGoldSetPayload() []byte {
	return []byte(`{"schema":1,"repository":"repo-ab","scan_policy_version":"scanner-v6","cases":[{"id":"case-ab","category":"concept","query":"private query","expectation":"relevant","judgments":[{"reference":{"path":"permitted/relative.go","start_line":1,"end_line":10},"relevance":3}]}]}`)
}

func replaceGold(payload, old, replacement []byte) []byte {
	return bytes.Replace(append([]byte(nil), payload...), old, replacement, 1)
}
