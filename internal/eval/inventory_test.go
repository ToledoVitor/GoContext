package eval_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	evaluation "github.com/ToledoVitor/GoContext/internal/eval"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/ingest/filesystem"
	"github.com/ToledoVitor/GoContext/internal/ingest/lineparser"
	"github.com/ToledoVitor/GoContext/internal/ingest/symbolchunker"
	"github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/search/lexical"
	"github.com/ToledoVitor/GoContext/internal/source"
	"github.com/ToledoVitor/GoContext/internal/testsupport/taintcheck"
)

type countingScanner struct {
	inner ingest.Scanner
	calls int
}

func (scanner *countingScanner) Scan(ctx context.Context, root string) (ingest.ScanResult, error) {
	scanner.calls++
	return scanner.inner.Scan(ctx, root)
}

type corpusLoader struct {
	repository string
	chunks     []source.Chunk
}

func (loader corpusLoader) Load(ctx context.Context, repository string) ([]source.Chunk, error) {
	if repository != loader.repository {
		return nil, context.Canceled
	}
	return append([]source.Chunk(nil), loader.chunks...), ctx.Err()
}

func lexicalFactory(repository string, chunks []source.Chunk) (search.Searcher, error) {
	return lexical.NewSearcher(corpusLoader{repository: repository, chunks: append([]source.Chunk(nil), chunks...)})
}

func TestEvaluateBuildsExactAggregateInventoryAndUsesOneTraversal(t *testing.T) {
	root := t.TempDir()
	writeEvalFile(t, root, "app.py", "def SharedName():\n    return 1\n")
	writeEvalFile(t, root, "tests/test_worker.py", "def UniqueName():\n    return 2\n")
	writeEvalFile(t, root, "migrations/001_change.ts", "export function SharedName() { return 3 }\n")
	writeEvalFile(t, root, "config.ts", "export const value = 1\n")
	writeEvalFile(t, root, "src/shared.js", "export function SharedName() { return 4 }\n")
	writeEvalFile(t, root, "src/view.jsx", "export const value = <main />\n")
	writeEvalFile(t, root, "README.md", "seven!!")

	scanner := &countingScanner{inner: filesystem.NewScanner()}
	report, err := evaluation.Evaluate(context.Background(), "repo-a1", root, evaluation.Dependencies{
		Scanner: scanner, Parser: lineparser.NewParser(), Chunker: symbolchunker.NewChunker(), SearchFactory: lexicalFactory,
	}, evaluation.Budgets{MaxEligibleFiles: 10, MaxEligibleBytes: 1 << 20, MaxAutoQueries: 10}, nil)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if scanner.calls != 1 {
		t.Fatalf("scanner calls = %d, want exactly one", scanner.calls)
	}
	if report.Decision != evaluation.DecisionGo {
		t.Fatalf("decision = %q; blockers = %#v", report.Decision, report.Blockers)
	}
	if report.Inventory.EligibleFiles != 6 || report.Inventory.IncludedFiles != 6 || report.Inventory.Chunks != 6 {
		t.Fatalf("inventory = %#v", report.Inventory)
	}
	if report.Inventory.SupportedExtensions[".py"] != 2 || report.Inventory.SupportedExtensions[".ts"] != 2 ||
		report.Inventory.SupportedExtensions[".js"] != 1 || report.Inventory.SupportedExtensions[".jsx"] != 1 {
		t.Fatalf("supported extensions = %#v", report.Inventory.SupportedExtensions)
	}
	if report.Inventory.SupportedLanguages[source.LanguageJavaScript] != 2 {
		t.Fatalf("supported languages = %#v, want two JavaScript files", report.Inventory.SupportedLanguages)
	}
	if got := report.Inventory.UnsupportedExtensions[".md"]; got.Files != 1 || got.Bytes != 7 {
		t.Fatalf("unsupported .md = %#v", got)
	}
	if report.Inventory.SymbolKinds[evaluation.SymbolFunction] != 4 || report.Inventory.FilesWithoutSymbols != 2 {
		t.Fatalf("symbol aggregates = %#v, files_without=%d", report.Inventory.SymbolKinds, report.Inventory.FilesWithoutSymbols)
	}
	if report.Inventory.PatternBuckets[evaluation.PatternTest] != 1 || report.Inventory.PatternBuckets[evaluation.PatternMigration] != 1 ||
		report.Inventory.PatternBuckets[evaluation.PatternConfiguration] != 1 {
		t.Fatalf("pattern buckets = %#v", report.Inventory.PatternBuckets)
	}
	metrics := report.Retrieval.Categories[evaluation.CategoryExactSymbol]
	if metrics.Status != evaluation.StatusEvaluated || metrics.QueryCount != 2 || metrics.RecallAt5 != 1 || metrics.RecallAt10 != 1 {
		t.Fatalf("exact-symbol metrics = %#v", metrics)
	}
	if report.CapabilityGaps[evaluation.CategoryFramework] != evaluation.StatusNotEvaluated {
		t.Fatalf("capability gaps = %#v", report.CapabilityGaps)
	}
	if _, err := evaluation.MarshalValidated(report); err != nil {
		t.Fatalf("MarshalValidated(report) error = %v", err)
	}
}

type fixedScanner struct {
	result ingest.ScanResult
	calls  int
}

func (scanner *fixedScanner) Scan(context.Context, string) (ingest.ScanResult, error) {
	scanner.calls++
	return scanner.result, nil
}

type countingParser struct{ calls int }

func (parser *countingParser) Parse(context.Context, source.File) ([]source.Symbol, error) {
	parser.calls++
	return nil, nil
}

type countingChunker struct{ calls int }

func (chunker *countingChunker) Chunk(context.Context, source.File, []source.Symbol) ([]source.Chunk, error) {
	chunker.calls++
	return nil, nil
}

func TestEvaluateEnforcesEligibleBudgetBeforeParseOrSearch(t *testing.T) {
	file := source.File{Reference: source.Reference{Path: "safe.py", StartLine: 1, EndLine: 1}, Language: source.LanguagePython, Content: []byte("x")}
	scanner := &fixedScanner{result: ingest.ScanResult{PolicyVersion: ingest.ScanPolicyVersion, Files: []source.File{file}, Report: ingest.ScanReport{
		EligibleFiles: 2, EligibleBytes: 2, IncludedFiles: 1, IncludedBytes: 1,
		Excluded: map[ingest.ExclusionReason]int{}, IncludedByLanguage: map[source.Language]int{source.LanguagePython: 1},
		SizeBands: map[string]int{"0-4KiB": 1}, UnsupportedByExtension: map[string]int{}, UnsupportedBytesByExtension: map[string]int64{},
	}}}
	parser := &countingParser{}
	chunker := &countingChunker{}
	factoryCalls := 0
	report, err := evaluation.Evaluate(context.Background(), "repo-a1", "ignored", evaluation.Dependencies{
		Scanner: scanner, Parser: parser, Chunker: chunker,
		SearchFactory: func(string, []source.Chunk) (search.Searcher, error) { factoryCalls++; return nil, nil },
	}, evaluation.Budgets{MaxEligibleFiles: 1, MaxEligibleBytes: 10, MaxAutoQueries: 1}, nil)
	if err == nil || report.Decision != evaluation.DecisionNoGo || report.Blockers[evaluation.BlockerBudget] != 1 {
		t.Fatalf("report = %#v; error = %v", report, err)
	}
	if scanner.calls != 1 || parser.calls != 0 || chunker.calls != 0 || factoryCalls != 0 {
		t.Fatalf("calls scanner/parser/chunker/factory = %d/%d/%d/%d", scanner.calls, parser.calls, chunker.calls, factoryCalls)
	}
}

type taintAuditingParser struct {
	t         *testing.T
	forbidden []string
	inner     ingest.Parser
}

func (parser taintAuditingParser) Parse(ctx context.Context, file source.File) ([]source.Symbol, error) {
	assertEvalTaintFree(parser.t, file, parser.forbidden)
	symbols, err := parser.inner.Parse(ctx, file)
	assertEvalTaintFree(parser.t, symbols, parser.forbidden)
	return symbols, err
}

type taintAuditingChunker struct {
	t         *testing.T
	forbidden []string
	inner     ingest.Chunker
}

func (chunker taintAuditingChunker) Chunk(ctx context.Context, file source.File, symbols []source.Symbol) ([]source.Chunk, error) {
	assertEvalTaintFree(chunker.t, file, chunker.forbidden)
	assertEvalTaintFree(chunker.t, symbols, chunker.forbidden)
	chunks, err := chunker.inner.Chunk(ctx, file, symbols)
	assertEvalTaintFree(chunker.t, chunks, chunker.forbidden)
	return chunks, err
}

type taintAuditingSearcher struct {
	t         *testing.T
	forbidden []string
	inner     search.Searcher
}

func (searcher taintAuditingSearcher) Search(ctx context.Context, query search.Query) ([]search.Hit, error) {
	assertEvalTaintFree(searcher.t, query, searcher.forbidden)
	hits, err := searcher.inner.Search(ctx, query)
	assertEvalTaintFree(searcher.t, hits, searcher.forbidden)
	return hits, err
}

func TestEvaluateHardDeniedCanariesReachNoParserChunkerSearchOrReport(t *testing.T) {
	root := t.TempDir()
	writeEvalFile(t, root, "safe.py", "def PublicSymbol():\n    return 1\n")
	writeEvalFile(t, root, ".env.py", "password = 'env-denied-CANARY'\n")
	writeEvalFile(t, root, ".github/workflow.ts", "export const value = 'workflow-denied-CANARY'\n")
	writeEvalFile(t, root, "credentials.ts", "export const value = 'credential-denied-CANARY'\n")
	forbidden := []string{"env-denied-CANARY", "workflow-denied-CANARY", "credential-denied-CANARY"}
	scanner := &countingScanner{inner: filesystem.NewScanner()}
	report, err := evaluation.Evaluate(context.Background(), "repo-a1", root, evaluation.Dependencies{
		Scanner: scanner,
		Parser:  taintAuditingParser{t: t, forbidden: forbidden, inner: lineparser.NewParser()},
		Chunker: taintAuditingChunker{t: t, forbidden: forbidden, inner: symbolchunker.NewChunker()},
		SearchFactory: func(repository string, chunks []source.Chunk) (search.Searcher, error) {
			assertEvalTaintFree(t, chunks, forbidden)
			inner, factoryErr := lexicalFactory(repository, chunks)
			return taintAuditingSearcher{t: t, forbidden: forbidden, inner: inner}, factoryErr
		},
	}, evaluation.Budgets{MaxEligibleFiles: 10, MaxEligibleBytes: 1 << 20, MaxAutoQueries: 10}, nil)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if scanner.calls != 1 {
		t.Fatalf("scanner calls = %d", scanner.calls)
	}
	assertEvalTaintFree(t, report, forbidden)
	payload, err := evaluation.MarshalValidated(report)
	if err != nil {
		t.Fatal(err)
	}
	assertEvalTaintFree(t, payload, forbidden)
}

func assertEvalTaintFree(t *testing.T, value any, forbidden []string) {
	t.Helper()
	result := taintcheck.ScanValue(value, forbidden)
	if !result.Complete || result.Found {
		t.Fatalf("taint result = %#v for %#v", result, value)
	}
}

type scannerFunc func(context.Context, string) (ingest.ScanResult, error)

func (function scannerFunc) Scan(ctx context.Context, root string) (ingest.ScanResult, error) {
	return function(ctx, root)
}

type parserFunc func(context.Context, source.File) ([]source.Symbol, error)

func (function parserFunc) Parse(ctx context.Context, file source.File) ([]source.Symbol, error) {
	return function(ctx, file)
}

type chunkerFunc func(context.Context, source.File, []source.Symbol) ([]source.Chunk, error)

func (function chunkerFunc) Chunk(ctx context.Context, file source.File, symbols []source.Symbol) ([]source.Chunk, error) {
	return function(ctx, file, symbols)
}

func TestEvaluateMapsPipelineFailuresToFixedCategoryWithoutPartialInventory(t *testing.T) {
	privateFailure := errors.New("private-path-symbol-content-CANARY")
	files := []source.File{
		{Reference: source.Reference{Path: "one.py", StartLine: 1, EndLine: 1}, Language: source.LanguagePython, Content: []byte("x")},
		{Reference: source.Reference{Path: "two.py", StartLine: 1, EndLine: 1}, Language: source.LanguagePython, Content: []byte("y")},
	}
	scanResult := validFixedScanResult(files)
	tests := []struct {
		name    string
		parser  ingest.Parser
		chunker ingest.Chunker
		factory evaluation.SearchFactory
	}{
		{
			name: "parse", parser: parserFunc(func(context.Context, source.File) ([]source.Symbol, error) { return nil, privateFailure }),
			chunker: chunkerFunc(func(context.Context, source.File, []source.Symbol) ([]source.Chunk, error) {
				t.Fatal("chunker called")
				return nil, nil
			}),
			factory: lexicalFactory,
		},
		{
			name: "chunk", parser: parserFunc(func(context.Context, source.File) ([]source.Symbol, error) { return nil, nil }),
			chunker: chunkerFunc(func(context.Context, source.File, []source.Symbol) ([]source.Chunk, error) {
				return nil, privateFailure
			}),
			factory: lexicalFactory,
		},
		{
			name: "corpus", parser: parserFunc(func(context.Context, source.File) ([]source.Symbol, error) { return nil, nil }),
			chunker: chunkerFunc(func(_ context.Context, file source.File, _ []source.Symbol) ([]source.Chunk, error) {
				return []source.Chunk{{ID: "duplicate", Text: "x", Language: file.Language, Reference: file.Reference}}, nil
			}),
			factory: lexicalFactory,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report, err := evaluation.Evaluate(context.Background(), "repo-a1", "ignored", evaluation.Dependencies{
				Scanner: &fixedScanner{result: scanResult}, Parser: test.parser, Chunker: test.chunker, SearchFactory: test.factory,
			}, evaluation.Budgets{MaxEligibleFiles: 10, MaxEligibleBytes: 10, MaxAutoQueries: 10}, nil)
			if err == nil || strings.Contains(err.Error(), "CANARY") || report.Decision != evaluation.DecisionNoGo ||
				report.Blockers[evaluation.BlockerIntegrity] != 1 || report.Inventory.IncludedFiles != 0 || report.Inventory.Chunks != 0 {
				t.Fatalf("report/error = %#v/%v", report, err)
			}
		})
	}
}

func TestEvaluateDeadlineCancelsScannerWithFixedNoGoCategory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := evaluation.Evaluate(ctx, "repo-a1", "ignored", evaluation.Dependencies{
		Scanner:       scannerFunc(func(ctx context.Context, _ string) (ingest.ScanResult, error) { return ingest.ScanResult{}, ctx.Err() }),
		Parser:        parserFunc(func(context.Context, source.File) ([]source.Symbol, error) { return nil, nil }),
		Chunker:       chunkerFunc(func(context.Context, source.File, []source.Symbol) ([]source.Chunk, error) { return nil, nil }),
		SearchFactory: lexicalFactory,
	}, evaluation.Budgets{MaxEligibleFiles: 1, MaxEligibleBytes: 1, MaxAutoQueries: 1}, nil)
	if err == nil || report.Blockers[evaluation.BlockerCanceled] != 1 {
		t.Fatalf("report/error = %#v/%v", report, err)
	}
}

func TestEvaluateStopsAfterSuccessfulDependencyCancelsContext(t *testing.T) {
	file := source.File{
		Reference: source.Reference{Path: "safe.py", StartLine: 1, EndLine: 1},
		Language:  source.LanguagePython,
		Content:   []byte("def Safe():"),
	}
	symbols := []source.Symbol{{
		Name: "Safe", Kind: "function", Reference: file.Reference,
	}}
	chunks := []source.Chunk{{
		ID: "safe", Text: "def Safe():", Language: file.Language, SymbolName: "Safe", Reference: file.Reference,
	}}
	for _, cancelStage := range []string{"scanner", "parser", "chunker", "factory"} {
		t.Run(cancelStage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			calls := map[string]int{}
			report, err := evaluation.Evaluate(ctx, "repo-a1", "ignored", evaluation.Dependencies{
				Scanner: scannerFunc(func(context.Context, string) (ingest.ScanResult, error) {
					calls["scanner"]++
					if cancelStage == "scanner" {
						cancel()
					}
					return validFixedScanResult([]source.File{file}), nil
				}),
				Parser: parserFunc(func(context.Context, source.File) ([]source.Symbol, error) {
					calls["parser"]++
					if cancelStage == "parser" {
						cancel()
					}
					return symbols, nil
				}),
				Chunker: chunkerFunc(func(context.Context, source.File, []source.Symbol) ([]source.Chunk, error) {
					calls["chunker"]++
					if cancelStage == "chunker" {
						cancel()
					}
					return chunks, nil
				}),
				SearchFactory: func(string, []source.Chunk) (search.Searcher, error) {
					calls["factory"]++
					if cancelStage == "factory" {
						cancel()
					}
					return searcherFunc(func(context.Context, search.Query) ([]search.Hit, error) {
						calls["search"]++
						return []search.Hit{{Chunk: chunks[0]}}, nil
					}), nil
				},
			}, evaluation.Budgets{MaxEligibleFiles: 10, MaxEligibleBytes: 100, MaxAutoQueries: 10}, nil)
			if err != context.Canceled || report.Blockers[evaluation.BlockerCanceled] != 1 {
				t.Fatalf("report/error = %#v/%v", report, err)
			}
			order := []string{"scanner", "parser", "chunker", "factory", "search"}
			cancelIndex := 0
			for index, stage := range order {
				if stage == cancelStage {
					cancelIndex = index
				}
			}
			for _, stage := range order[cancelIndex+1:] {
				if calls[stage] != 0 {
					t.Fatalf("%s called after %s canceled context: %#v", stage, cancelStage, calls)
				}
			}
		})
	}
}

func TestEvaluateRejectsChunkerThatOmitsParsedSymbol(t *testing.T) {
	file := source.File{
		Reference: source.Reference{Path: "symbols.py", StartLine: 1, EndLine: 2}, Language: source.LanguagePython,
		Content: []byte("def One():\ndef Two():"),
	}
	symbols := []source.Symbol{
		{Name: "One", Kind: "function", Reference: source.Reference{Path: "symbols.py", StartLine: 1, EndLine: 1}},
		{Name: "Two", Kind: "function", Reference: source.Reference{Path: "symbols.py", StartLine: 2, EndLine: 2}},
	}
	report, err := evaluation.Evaluate(context.Background(), "repo-a1", "ignored", evaluation.Dependencies{
		Scanner: &fixedScanner{result: validFixedScanResult([]source.File{file})},
		Parser:  parserFunc(func(context.Context, source.File) ([]source.Symbol, error) { return symbols, nil }),
		Chunker: chunkerFunc(func(context.Context, source.File, []source.Symbol) ([]source.Chunk, error) {
			return []source.Chunk{{
				ID: "only-one", Text: "def One():", Language: source.LanguagePython, SymbolName: "One",
				Reference: source.Reference{Path: "symbols.py", StartLine: 1, EndLine: 1},
			}}, nil
		}),
		SearchFactory: lexicalFactory,
	}, evaluation.Budgets{MaxEligibleFiles: 10, MaxEligibleBytes: 100, MaxAutoQueries: 10}, nil)
	if err == nil || report.Blockers[evaluation.BlockerIntegrity] != 1 {
		t.Fatalf("report/error = %#v/%v", report, err)
	}
}

func TestEvaluateCapsPrivateExactSymbolCasesAtChecklistBudget(t *testing.T) {
	root := t.TempDir()
	writeEvalFile(t, root, "symbols.py", "def FirstSymbol():\n    return 1\ndef SecondSymbol():\n    return 2\n")
	searchCalls := 0
	report, err := evaluation.Evaluate(context.Background(), "repo-a1", root, evaluation.Dependencies{
		Scanner: filesystem.NewScanner(), Parser: lineparser.NewParser(), Chunker: symbolchunker.NewChunker(),
		SearchFactory: func(repository string, chunks []source.Chunk) (search.Searcher, error) {
			inner, factoryErr := lexicalFactory(repository, chunks)
			return searcherFunc(func(ctx context.Context, query search.Query) ([]search.Hit, error) {
				searchCalls++
				return inner.Search(ctx, query)
			}), factoryErr
		},
	}, evaluation.Budgets{MaxEligibleFiles: 10, MaxEligibleBytes: 1 << 20, MaxAutoQueries: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	metrics := report.Retrieval.Categories[evaluation.CategoryExactSymbol]
	if metrics.QueryCount != 1 || searchCalls != 2 {
		t.Fatalf("query count/search calls = %d/%d", metrics.QueryCount, searchCalls)
	}
}

func TestEvaluateResolvesHumanGoldReferenceIntoCanonicalChunk(t *testing.T) {
	file := source.File{Reference: source.Reference{Path: "permitted/relative.py", StartLine: 1, EndLine: 10}, Language: source.LanguagePython, Content: []byte("content")}
	chunk := source.Chunk{ID: "opaque-chunk", Text: "content", Language: source.LanguagePython, Reference: file.Reference}
	payload := bytes.ReplaceAll(validGoldSetPayload(), []byte("repo-ab"), []byte("repo-a1"))
	payload = bytes.ReplaceAll(payload, []byte("permitted/relative.go"), []byte("permitted/relative.py"))
	goldSet, err := evaluation.ParseGoldSet(context.Background(), payload, "repo-a1", ingest.ScanPolicyVersion)
	if err != nil {
		t.Fatal(err)
	}
	searcher := &sequenceSearcher{results: [][]search.Hit{{{Chunk: chunk}}, {{Chunk: chunk}}}}
	report, err := evaluation.Evaluate(context.Background(), "repo-a1", "ignored", evaluation.Dependencies{
		Scanner: &fixedScanner{result: validFixedScanResult([]source.File{file})},
		Parser:  parserFunc(func(context.Context, source.File) ([]source.Symbol, error) { return nil, nil }),
		Chunker: chunkerFunc(func(context.Context, source.File, []source.Symbol) ([]source.Chunk, error) {
			return []source.Chunk{chunk}, nil
		}),
		SearchFactory: func(string, []source.Chunk) (search.Searcher, error) { return searcher, nil },
	}, evaluation.Budgets{MaxEligibleFiles: 10, MaxEligibleBytes: 100, MaxAutoQueries: 10}, goldSet)
	if err != nil {
		t.Fatalf("Evaluate() report/error = %#v/%v", report, err)
	}
	metrics := report.Retrieval.Categories[evaluation.CategoryConcept]
	if metrics.Status != evaluation.StatusEvaluated || metrics.QueryCount != 1 || metrics.RecallAt5 != 1 ||
		report.CapabilityGaps[evaluation.CategoryConcept] != evaluation.StatusEvaluated ||
		report.Retrieval.Categories[evaluation.CategoryExactSymbol].Status != evaluation.StatusNotEvaluated ||
		report.Limitations[evaluation.LimitationAutomaticSymbols] != 0 {
		t.Fatalf("report = %#v", report)
	}
}

func TestEvaluateRejectsMissingOrAmbiguousGoldReferenceBeforeSearchFactory(t *testing.T) {
	file := source.File{Reference: source.Reference{Path: "permitted/relative.py", StartLine: 1, EndLine: 10}, Language: source.LanguagePython, Content: []byte("content")}
	goldPayload := bytes.ReplaceAll(validGoldSetPayload(), []byte("repo-ab"), []byte("repo-a1"))
	goldPayload = bytes.ReplaceAll(goldPayload, []byte("permitted/relative.go"), []byte("permitted/relative.py"))
	goldSet, err := evaluation.ParseGoldSet(context.Background(), goldPayload, "repo-a1", ingest.ScanPolicyVersion)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		chunks []source.Chunk
	}{
		{name: "missing", chunks: []source.Chunk{{ID: "missing", Text: "content", Language: source.LanguagePython, Reference: source.Reference{Path: file.Reference.Path, StartLine: 1, EndLine: 5}}}},
		{name: "ambiguous", chunks: []source.Chunk{
			{ID: "one", SymbolName: "One", Text: "one", Language: source.LanguagePython, Reference: file.Reference},
			{ID: "two", SymbolName: "Two", Text: "two", Language: source.LanguagePython, Reference: file.Reference},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			factoryCalls := 0
			parser := parserFunc(func(context.Context, source.File) ([]source.Symbol, error) { return nil, nil })
			if test.name == "ambiguous" {
				parser = parserFunc(func(context.Context, source.File) ([]source.Symbol, error) {
					return []source.Symbol{
						{Name: "One", Kind: "function", Reference: source.Reference{Path: file.Reference.Path, StartLine: 1, EndLine: 1}},
						{Name: "Two", Kind: "function", Reference: source.Reference{Path: file.Reference.Path, StartLine: 2, EndLine: 2}},
					}, nil
				})
			}
			report, evalErr := evaluation.Evaluate(context.Background(), "repo-a1", "ignored", evaluation.Dependencies{
				Scanner: &fixedScanner{result: validFixedScanResult([]source.File{file})}, Parser: parser,
				Chunker:       chunkerFunc(func(context.Context, source.File, []source.Symbol) ([]source.Chunk, error) { return test.chunks, nil }),
				SearchFactory: func(string, []source.Chunk) (search.Searcher, error) { factoryCalls++; return nil, nil },
			}, evaluation.Budgets{MaxEligibleFiles: 10, MaxEligibleBytes: 100, MaxAutoQueries: 10}, goldSet)
			if evalErr == nil || report.Blockers[evaluation.BlockerIntegrity] != 1 || factoryCalls != 0 {
				t.Fatalf("report/error/factory = %#v/%v/%d", report, evalErr, factoryCalls)
			}
		})
	}
}

func TestEvaluateRejectsGoldCasesOverChecklistQueryBudgetBeforeSearch(t *testing.T) {
	file := source.File{Reference: source.Reference{Path: "safe.py", StartLine: 1, EndLine: 1}, Language: source.LanguagePython, Content: []byte("content")}
	chunk := source.Chunk{ID: "opaque", Text: "content", Language: source.LanguagePython, Reference: file.Reference}
	payload := []byte(`{"schema":1,"repository":"repo-a1","scan_policy_version":"scanner-v6","cases":[` +
		`{"id":"case-a1","category":"concept","query":"first","expectation":"relevant","judgments":[{"reference":{"path":"safe.py","start_line":1,"end_line":1},"relevance":1}]},` +
		`{"id":"case-a2","category":"framework","query":"second","expectation":"relevant","judgments":[{"reference":{"path":"safe.py","start_line":1,"end_line":1},"relevance":1}]}]}`)
	goldSet, err := evaluation.ParseGoldSet(context.Background(), payload, "repo-a1", ingest.ScanPolicyVersion)
	if err != nil {
		t.Fatal(err)
	}
	factoryCalls := 0
	report, evalErr := evaluation.Evaluate(context.Background(), "repo-a1", "ignored", evaluation.Dependencies{
		Scanner: &fixedScanner{result: validFixedScanResult([]source.File{file})},
		Parser:  parserFunc(func(context.Context, source.File) ([]source.Symbol, error) { return nil, nil }),
		Chunker: chunkerFunc(func(context.Context, source.File, []source.Symbol) ([]source.Chunk, error) {
			return []source.Chunk{chunk}, nil
		}),
		SearchFactory: func(string, []source.Chunk) (search.Searcher, error) { factoryCalls++; return nil, nil },
	}, evaluation.Budgets{MaxEligibleFiles: 10, MaxEligibleBytes: 100, MaxAutoQueries: 1}, goldSet)
	if evalErr == nil || report.Blockers[evaluation.BlockerBudget] != 1 || factoryCalls != 0 {
		t.Fatalf("report/error/factory = %#v/%v/%d", report, evalErr, factoryCalls)
	}
}

type searcherFunc func(context.Context, search.Query) ([]search.Hit, error)

func (function searcherFunc) Search(ctx context.Context, query search.Query) ([]search.Hit, error) {
	return function(ctx, query)
}

func validFixedScanResult(files []source.File) ingest.ScanResult {
	result := ingest.ScanResult{PolicyVersion: ingest.ScanPolicyVersion, Files: files, Report: ingest.ScanReport{
		Excluded: map[ingest.ExclusionReason]int{}, IncludedByLanguage: map[source.Language]int{},
		SizeBands: map[string]int{}, UnsupportedByExtension: map[string]int{}, UnsupportedBytesByExtension: map[string]int64{},
	}}
	for _, file := range files {
		result.Report.EligibleFiles++
		result.Report.IncludedFiles++
		result.Report.EligibleBytes += int64(len(file.Content))
		result.Report.IncludedBytes += int64(len(file.Content))
		result.Report.IncludedByLanguage[file.Language]++
		result.Report.SizeBands["0-4KiB"]++
	}
	return result
}

func writeEvalFile(t *testing.T, root, relative string, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
