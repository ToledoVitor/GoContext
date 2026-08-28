package eval_test

import (
	"bytes"
	"encoding/json"
	"testing"

	evaluation "github.com/ToledoVitor/GoContext/internal/eval"
	"github.com/ToledoVitor/GoContext/internal/testsupport/taintcheck"
)

func TestMarshalValidatedEmitsOnlyAggregateAllowlistedSchema(t *testing.T) {
	report := evaluation.EmptyReport("repo-a1", evaluation.DecisionGo)
	report.Inventory.EligibleFiles = 2
	report.Inventory.EligibleBytes = 144
	report.Inventory.IncludedFiles = 2
	report.Inventory.IncludedBytes = 144
	report.Inventory.SupportedLanguages["python"] = 2
	report.Inventory.SupportedExtensions[".py"] = 2
	report.Inventory.SizeBands[evaluation.SizeBand0To4KiB] = 2
	report.Inventory.SymbolKinds[evaluation.SymbolFunction] = 2
	report.Retrieval.Categories[evaluation.CategoryExactSymbol] = evaluation.CategoryMetrics{
		Status: evaluation.StatusEvaluated, QueryCount: 2, RecallAt5: 1, RecallAt10: 1,
		MRRAt10: 1, NDCGAt10: 1,
	}
	report.Retrieval.Status = evaluation.StatusEvaluated
	report.Retrieval.CitationValidity = 1
	report.Retrieval.DeterministicOrderRate = 1
	report.Retrieval.FallbackReason = evaluation.FallbackLexicalBaseline

	payload, err := evaluation.MarshalValidated(report)
	if err != nil {
		t.Fatalf("MarshalValidated() error = %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	wantFields := []string{"schema", "repository", "decision", "blockers", "inventory", "retrieval", "performance", "capability_gaps", "limitations"}
	if len(envelope) != len(wantFields) {
		t.Fatalf("top-level fields = %v", envelope)
	}
	for _, field := range wantFields {
		if _, ok := envelope[field]; !ok {
			t.Fatalf("missing field %q", field)
		}
	}

	forbidden := []string{
		"private/source/CANARY.ts", "ExactSecretSymbolCANARY", "query CANARY", "chunk-CANARY",
		"https://user-CANARY.invalid", "person-CANARY", "host-CANARY", "output-CANARY",
	}
	result := taintcheck.Scan(payload, forbidden)
	if !result.Complete || result.Found {
		t.Fatalf("sanitized payload taint result = %#v; payload = %s", result, payload)
	}
	for _, forbiddenField := range []string{"root", "path", "source", "symbol", "query", "chunk_id", "hit", "rank", "timestamp", "error"} {
		if bytes.Contains(payload, []byte(`"`+forbiddenField+`"`)) {
			t.Fatalf("payload contains forbidden field %q: %s", forbiddenField, payload)
		}
	}
}

func TestMarshalValidatedRejectsNonOpaqueRepositoryAndUnknownMapCategories(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*evaluation.Report)
	}{
		{name: "repository", mutate: func(report *evaluation.Report) { report.Repository = "/private/repository" }},
		{name: "blocker", mutate: func(report *evaluation.Report) { report.Blockers["private-path"] = 1 }},
		{name: "language", mutate: func(report *evaluation.Report) { report.Inventory.SupportedLanguages["private-language"] = 1 }},
		{name: "category", mutate: func(report *evaluation.Report) {
			report.Retrieval.Categories["private-query"] = evaluation.CategoryMetrics{}
		}},
		{name: "recall above one", mutate: func(report *evaluation.Report) {
			metrics := report.Retrieval.Categories[evaluation.CategoryExactSymbol]
			metrics.Status = evaluation.StatusEvaluated
			metrics.QueryCount = 1
			metrics.RecallAt5 = 1.01
			report.Retrieval.Categories[evaluation.CategoryExactSymbol] = metrics
		}},
		{name: "ndcg above one", mutate: func(report *evaluation.Report) {
			metrics := report.Retrieval.Categories[evaluation.CategoryExactSymbol]
			metrics.Status = evaluation.StatusEvaluated
			metrics.QueryCount = 1
			metrics.NDCGAt10 = 1.01
			report.Retrieval.Categories[evaluation.CategoryExactSymbol] = metrics
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := evaluation.EmptyReport("repo-a1", evaluation.DecisionNoGo)
			test.mutate(&report)
			if _, err := evaluation.MarshalValidated(report); err == nil {
				t.Fatal("MarshalValidated() error = nil, want sanitized validation failure")
			}
		})
	}
}
