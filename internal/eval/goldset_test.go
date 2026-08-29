package eval_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

func TestParseGoldSetRequiresFiveDistinctCasesPerCategory(t *testing.T) {
	for count := 1; count < 5; count++ {
		t.Run(fmt.Sprintf("concept count %d", count), func(t *testing.T) {
			payload := goldSetPayload(goldCategoryFixture{category: evaluation.CategoryConcept, count: count})
			goldSet, err := evaluation.ParseGoldSet(context.Background(), payload, "repo-ab", ingest.ScanPolicyVersion)
			if goldSet != nil || !errors.Is(err, evaluation.ErrGoldSet) {
				t.Fatalf("ParseGoldSet() = %#v, %v", goldSet, err)
			}
		})
	}

	tests := []struct {
		name       string
		categories []goldCategoryFixture
		valid      bool
	}{
		{name: "five concept", categories: []goldCategoryFixture{{category: evaluation.CategoryConcept, count: 5}}, valid: true},
		{name: "mixed underfilled", categories: []goldCategoryFixture{{category: evaluation.CategoryConcept, count: 5}, {category: evaluation.CategoryFramework, count: 4}}},
		{name: "mixed complete", categories: []goldCategoryFixture{{category: evaluation.CategoryConcept, count: 5}, {category: evaluation.CategoryFramework, count: 5}}, valid: true},
		{name: "four negative evidence", categories: []goldCategoryFixture{{category: evaluation.CategoryNegativeEvidence, count: 4}}},
		{name: "five negative evidence", categories: []goldCategoryFixture{{category: evaluation.CategoryNegativeEvidence, count: 5}}, valid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := goldSetPayload(test.categories...)
			goldSet, err := evaluation.ParseGoldSet(context.Background(), payload, "repo-ab", ingest.ScanPolicyVersion)
			if test.valid {
				if goldSet == nil || err != nil {
					t.Fatalf("ParseGoldSet() = %#v, %v", goldSet, err)
				}
				return
			}
			if goldSet != nil || !errors.Is(err, evaluation.ErrGoldSet) {
				t.Fatalf("ParseGoldSet() = %#v, %v", goldSet, err)
			}
		})
	}
}

func TestParseGoldSetPreservesThousandCaseUpperBound(t *testing.T) {
	tests := []struct {
		count int
		valid bool
	}{
		{count: 1000, valid: true},
		{count: 1001},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("count %d", test.count), func(t *testing.T) {
			payload := goldSetPayload(goldCategoryFixture{category: evaluation.CategoryConcept, count: test.count})
			goldSet, err := evaluation.ParseGoldSet(context.Background(), payload, "repo-ab", ingest.ScanPolicyVersion)
			if test.valid {
				if goldSet == nil || err != nil {
					t.Fatalf("ParseGoldSet() = %#v, %v", goldSet, err)
				}
				return
			}
			if goldSet != nil || !errors.Is(err, evaluation.ErrGoldSet) {
				t.Fatalf("ParseGoldSet() = %#v, %v", goldSet, err)
			}
		})
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
		{name: "query too long", payload: replaceGold(validGoldSetPayload(), []byte(`"private query 1"`), []byte(`"`+strings.Repeat("q", 4097)+`"`)), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "whitespace query", payload: replaceGold(validGoldSetPayload(), []byte(`"private query 1"`), []byte(`"   "`)), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "missing judgment", payload: replaceGold(validGoldSetPayload(), []byte(`"judgments":[{"reference":{"path":"permitted/relative.go","start_line":1,"end_line":10},"relevance":3}]`), []byte(`"judgments":[]`)), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "bad relevance", payload: replaceGold(validGoldSetPayload(), []byte(`"relevance":3`), []byte(`"relevance":4`)), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "unknown category", payload: replaceGold(validGoldSetPayload(), []byte(`"concept"`), []byte(`"private-category"`)), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "repository mismatch", payload: validGoldSetPayload(), repo: "repo-cd", policy: ingest.ScanPolicyVersion},
		{name: "policy mismatch", payload: validGoldSetPayload(), repo: "repo-ab", policy: "scanner-old"},
		{name: "unknown key", payload: replaceGold(validGoldSetPayload(), []byte(`"schema":1`), []byte(`"schema":1,"private":"`+canary+`"`)), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "missing key", payload: replaceGold(validGoldSetPayload(), []byte(`"id":"case-01",`), nil), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "duplicate key", payload: replaceGold(validGoldSetPayload(), []byte(`"id":"case-01"`), []byte(`"id":"case-01","id":"`+canary+`"`)), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "trailing json", payload: append(validGoldSetPayload(), []byte(` {}`)...), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
		{name: "malformed json", payload: []byte(`{"schema":`), repo: "repo-ab", policy: ingest.ScanPolicyVersion},
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

func TestParseGoldSetRejectsExcessiveJSONDepth(t *testing.T) {
	payload := []byte(strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34))
	if _, err := evaluation.ParseGoldSet(context.Background(), payload, "repo-ab", ingest.ScanPolicyVersion); !errors.Is(err, evaluation.ErrGoldSet) {
		t.Fatalf("ParseGoldSet() error = %v", err)
	}
}

func TestParseGoldSetRejectsDuplicatePrivateCaseFacts(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "id", payload: replaceGold(validGoldSetPayload(), []byte(`"id":"case-02"`), []byte(`"id":"case-01"`))},
		{name: "category query", payload: replaceGold(validGoldSetPayload(), []byte(`"query":"private query 2"`), []byte(`"query":"private query 1"`))},
		{name: "reference", payload: replaceGold(validGoldSetPayload(), []byte(`"judgments":[`), []byte(`"judgments":[{"reference":{"path":"permitted/relative.go","start_line":1,"end_line":10},"relevance":2},`))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := evaluation.ParseGoldSet(context.Background(), test.payload, "repo-ab", ingest.ScanPolicyVersion); !errors.Is(err, evaluation.ErrGoldSet) {
				t.Fatalf("ParseGoldSet() error = %v", err)
			}
		})
	}
}

func TestParseGoldSetEnforcesNegativeEvidenceShapeAndCancellation(t *testing.T) {
	negative := goldSetPayload(goldCategoryFixture{category: evaluation.CategoryNegativeEvidence, count: 5})
	if value, err := evaluation.ParseGoldSet(context.Background(), negative, "repo-ab", ingest.ScanPolicyVersion); err != nil || value == nil {
		t.Fatalf("negative ParseGoldSet() = %#v, %v", value, err)
	}
	for _, payload := range [][]byte{
		bytes.ReplaceAll(negative, []byte(`"negative_evidence"`), []byte(`"concept"`)),
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
	return goldSetPayload(goldCategoryFixture{category: evaluation.CategoryConcept, count: 5})
}

type goldCategoryFixture struct {
	category evaluation.QueryCategory
	count    int
}

func goldSetPayload(categories ...goldCategoryFixture) []byte {
	var payload strings.Builder
	payload.WriteString(`{"schema":1,"repository":"repo-ab","scan_policy_version":"scanner-v6","cases":[`)
	caseNumber := 0
	for _, category := range categories {
		for index := 0; index < category.count; index++ {
			if caseNumber > 0 {
				payload.WriteByte(',')
			}
			caseNumber++
			if category.category == evaluation.CategoryNegativeEvidence {
				fmt.Fprintf(&payload, `{"id":"case-%02d","category":%q,"query":"private query %d","expectation":"no_evidence","judgments":[]}`, caseNumber, category.category, caseNumber)
				continue
			}
			fmt.Fprintf(&payload, `{"id":"case-%02d","category":%q,"query":"private query %d","expectation":"relevant","judgments":[{"reference":{"path":"permitted/relative.go","start_line":1,"end_line":10},"relevance":3}]}`, caseNumber, category.category, caseNumber)
		}
	}
	payload.WriteString(`]}`)
	return []byte(payload.String())
}

func replaceGold(payload, old, replacement []byte) []byte {
	return bytes.Replace(append([]byte(nil), payload...), old, replacement, 1)
}
