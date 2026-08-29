package eval_test

import (
	"context"
	"math"
	"testing"
	"time"

	evaluation "github.com/ToledoVitor/GoContext/internal/eval"
	"github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/source"
)

type sequenceSearcher struct {
	results [][]search.Hit
	calls   int
	cancel  context.CancelFunc
}

func (searcher *sequenceSearcher) Search(ctx context.Context, query search.Query) ([]search.Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := searcher.results[searcher.calls]
	searcher.calls++
	if searcher.cancel != nil {
		searcher.cancel()
	}
	return append([]search.Hit(nil), result...), nil
}

func TestEvaluateMetricsComputesHandWorkedDuplicateRelevanceAndPercentiles(t *testing.T) {
	canonical := []source.Chunk{
		metricChunk("a", "one.py", 1), metricChunk("b", "two.py", 2), metricChunk("x", "other.py", 3),
	}
	hits := []search.Hit{{Chunk: canonical[1]}, {Chunk: canonical[2]}, {Chunk: canonical[0]}}
	searcher := &sequenceSearcher{results: [][]search.Hit{hits, hits}}
	clock := scriptedClock(
		time.Unix(0, 0), time.Unix(0, 100*time.Microsecond.Nanoseconds()),
		time.Unix(0, 200*time.Microsecond.Nanoseconds()), time.Unix(0, 500*time.Microsecond.Nanoseconds()),
	)

	result, err := evaluation.EvaluateMetrics(context.Background(), searcher, "repo-a1", canonical, []evaluation.Case{{
		Category: evaluation.CategoryExactSymbol, Query: "duplicate", RelevantChunkIDs: []string{"a", "b"},
	}}, evaluation.MetricOptions{Now: clock})
	if err != nil {
		t.Fatalf("EvaluateMetrics() error = %v", err)
	}
	metrics := result.Report.Categories[evaluation.CategoryExactSymbol]
	if metrics.QueryCount != 1 || metrics.RecallAt5 != 1 || metrics.RecallAt10 != 1 || metrics.MRRAt10 != 1 {
		t.Fatalf("metrics = %#v", metrics)
	}
	wantNDCG := 1.5 / (1 + 1/math.Log2(3))
	if math.Abs(metrics.NDCGAt10-wantNDCG) > 1e-12 {
		t.Fatalf("NDCG@10 = %.15f, want %.15f", metrics.NDCGAt10, wantNDCG)
	}
	if result.Report.CitationValidity != 1 || result.Report.DeterministicOrderRate != 1 {
		t.Fatalf("retrieval aggregate = %#v", result.Report)
	}
	if result.QueryP50Micros != 100 || result.QueryP95Micros != 300 {
		t.Fatalf("latencies = p50 %d p95 %d, want 100/300", result.QueryP50Micros, result.QueryP95Micros)
	}
	if searcher.calls != 2 {
		t.Fatalf("search calls = %d, want two deterministic runs", searcher.calls)
	}
}

func TestEvaluateMetricsDetectsForgedCitationAndNondeterministicCompleteOrder(t *testing.T) {
	canonical := []source.Chunk{metricChunk("a", "one.py", 1), metricChunk("b", "two.py", 2)}
	forged := canonical[0]
	forged.Reference.Path = "forged.py"
	searcher := &sequenceSearcher{results: [][]search.Hit{
		{{Chunk: forged}, {Chunk: canonical[1]}},
		{{Chunk: canonical[1]}, {Chunk: canonical[0]}},
	}}

	result, err := evaluation.EvaluateMetrics(context.Background(), searcher, "repo-a1", canonical, []evaluation.Case{{
		Category: evaluation.CategoryExactSymbol, Query: "symbol", RelevantChunkIDs: []string{"a"},
	}}, evaluation.MetricOptions{})
	if err != nil {
		t.Fatalf("EvaluateMetrics() error = %v", err)
	}
	if result.Report.CitationValidity != 0.75 {
		t.Fatalf("citation validity = %v, want 3/4", result.Report.CitationValidity)
	}
	if result.Report.DeterministicOrderRate != 0 {
		t.Fatalf("deterministic order = %v, want 0", result.Report.DeterministicOrderRate)
	}
}

func TestEvaluateMetricsLeavesQualityNotEvaluatedWithoutRelevantCases(t *testing.T) {
	searcher := &sequenceSearcher{}
	result, err := evaluation.EvaluateMetrics(context.Background(), searcher, "repo-a1", []source.Chunk{metricChunk("a", "one.py", 1)}, []evaluation.Case{{
		Category: evaluation.CategoryExactSymbol, Query: "negative", RelevantChunkIDs: nil,
	}}, evaluation.MetricOptions{})
	if err != nil {
		t.Fatalf("EvaluateMetrics() error = %v", err)
	}
	if result.Report.Status != evaluation.StatusNotEvaluated || searcher.calls != 0 {
		t.Fatalf("result = %#v; calls = %d", result, searcher.calls)
	}
}

func TestEvaluateMetricsHonorsCancellationBeforeSearch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	searcher := &sequenceSearcher{}
	_, err := evaluation.EvaluateMetrics(ctx, searcher, "repo-a1", []source.Chunk{metricChunk("a", "one.py", 1)}, []evaluation.Case{{
		Category: evaluation.CategoryExactSymbol, Query: "symbol", RelevantChunkIDs: []string{"a"},
	}}, evaluation.MetricOptions{})
	if err == nil || searcher.calls != 0 {
		t.Fatalf("error = %v; calls = %d", err, searcher.calls)
	}
}

func TestEvaluateMetricsNoHitsProducesZeroQualityAndValidEmptyCitations(t *testing.T) {
	searcher := &sequenceSearcher{results: [][]search.Hit{nil, nil}}
	result, err := evaluation.EvaluateMetrics(context.Background(), searcher, "repo-a1", []source.Chunk{metricChunk("a", "one.py", 1)}, []evaluation.Case{{
		Category: evaluation.CategoryExactSymbol, Query: "missing", RelevantChunkIDs: []string{"a"},
	}}, evaluation.MetricOptions{})
	if err != nil {
		t.Fatal(err)
	}
	metrics := result.Report.Categories[evaluation.CategoryExactSymbol]
	if metrics.RecallAt5 != 0 || metrics.RecallAt10 != 0 || metrics.MRRAt10 != 0 || metrics.NDCGAt10 != 0 ||
		result.Report.CitationValidity != 1 || result.Report.DeterministicOrderRate != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestEvaluateMetricsRejectsUnknownRelevantIDAndSearcherLimitViolation(t *testing.T) {
	canonical := []source.Chunk{metricChunk("a", "one.py", 1)}
	searcher := &sequenceSearcher{}
	_, err := evaluation.EvaluateMetrics(context.Background(), searcher, "repo-a1", canonical, []evaluation.Case{{
		Category: evaluation.CategoryExactSymbol, Query: "symbol", RelevantChunkIDs: []string{"unknown"},
	}}, evaluation.MetricOptions{})
	if err == nil || searcher.calls != 0 {
		t.Fatalf("unknown relevant error/calls = %v/%d", err, searcher.calls)
	}

	hits := make([]search.Hit, 11)
	for index := range hits {
		chunk := metricChunk(string(rune('b'+index)), "many.py", index+1)
		canonical = append(canonical, chunk)
		hits[index] = search.Hit{Chunk: chunk, Score: 1}
	}
	searcher = &sequenceSearcher{results: [][]search.Hit{hits}}
	_, err = evaluation.EvaluateMetrics(context.Background(), searcher, "repo-a1", canonical, []evaluation.Case{{
		Category: evaluation.CategoryExactSymbol, Query: "symbol", RelevantChunkIDs: []string{"a"},
	}}, evaluation.MetricOptions{})
	if err == nil || searcher.calls != 1 {
		t.Fatalf("limit violation error/calls = %v/%d", err, searcher.calls)
	}
}

func TestEvaluateMetricsRejectsDuplicateSearchHitIDsBeforeComputingQuality(t *testing.T) {
	canonical := []source.Chunk{metricChunk("a", "one.py", 1)}
	duplicate := []search.Hit{{Chunk: canonical[0]}, {Chunk: canonical[0]}}
	searcher := &sequenceSearcher{results: [][]search.Hit{duplicate}}

	_, err := evaluation.EvaluateMetrics(context.Background(), searcher, "repo-a1", canonical, []evaluation.Case{{
		Category: evaluation.CategoryExactSymbol, Query: "symbol", RelevantChunkIDs: []string{"a"},
	}}, evaluation.MetricOptions{})
	if err == nil || searcher.calls != 1 {
		t.Fatalf("duplicate hit error/calls = %v/%d", err, searcher.calls)
	}
}

func TestEvaluateMetricsQualityRatiosRemainBounded(t *testing.T) {
	canonical := []source.Chunk{
		metricChunk("a", "one.py", 1), metricChunk("b", "two.py", 2), metricChunk("x", "other.py", 3),
	}
	hits := []search.Hit{{Chunk: canonical[0]}, {Chunk: canonical[2]}, {Chunk: canonical[1]}}
	result, err := evaluation.EvaluateMetrics(context.Background(), &sequenceSearcher{results: [][]search.Hit{hits, hits}}, "repo-a1", canonical, []evaluation.Case{{
		Category: evaluation.CategoryExactSymbol, Query: "symbol", RelevantChunkIDs: []string{"a", "b"},
	}}, evaluation.MetricOptions{})
	if err != nil {
		t.Fatal(err)
	}
	metrics := result.Report.Categories[evaluation.CategoryExactSymbol]
	for name, value := range map[string]float64{
		"recall@5": metrics.RecallAt5, "recall@10": metrics.RecallAt10,
		"mrr@10": metrics.MRRAt10, "ndcg@10": metrics.NDCGAt10,
	} {
		if value < 0 || value > 1 {
			t.Fatalf("%s = %v, want [0,1]", name, value)
		}
	}
}

func TestEvaluateMetricsReturnsCancellationAfterSuccessfulSearch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	canonical := []source.Chunk{metricChunk("a", "one.py", 1)}
	searcher := &sequenceSearcher{results: [][]search.Hit{{{Chunk: canonical[0]}}}, cancel: cancel}

	_, err := evaluation.EvaluateMetrics(ctx, searcher, "repo-a1", canonical, []evaluation.Case{{
		Category: evaluation.CategoryExactSymbol, Query: "symbol", RelevantChunkIDs: []string{"a"},
	}}, evaluation.MetricOptions{})
	if err != context.Canceled || searcher.calls != 1 {
		t.Fatalf("post-search cancellation error/calls = %v/%d", err, searcher.calls)
	}
}

func TestEvaluateMetricsUsesCompleteDeterministicRankOrderForScoreTies(t *testing.T) {
	canonical := []source.Chunk{metricChunk("a", "one.py", 1), metricChunk("x", "other.py", 2)}
	tied := []search.Hit{{Chunk: canonical[1], Score: 1}, {Chunk: canonical[0], Score: 1}}
	searcher := &sequenceSearcher{results: [][]search.Hit{tied, tied}}
	result, err := evaluation.EvaluateMetrics(context.Background(), searcher, "repo-a1", canonical, []evaluation.Case{{
		Category: evaluation.CategoryExactSymbol, Query: "symbol", RelevantChunkIDs: []string{"a"},
	}}, evaluation.MetricOptions{})
	if err != nil {
		t.Fatal(err)
	}
	metrics := result.Report.Categories[evaluation.CategoryExactSymbol]
	if metrics.MRRAt10 != 0.5 || math.Abs(metrics.NDCGAt10-1/math.Log2(3)) > 1e-12 || result.Report.DeterministicOrderRate != 1 {
		t.Fatalf("metrics = %#v; determinism = %v", metrics, result.Report.DeterministicOrderRate)
	}
}

func TestEvaluateMetricsComputesGradedNDCGFromPrivateRelevance(t *testing.T) {
	canonical := []source.Chunk{metricChunk("high", "high.py", 1), metricChunk("low", "low.py", 1)}
	hits := []search.Hit{{Chunk: canonical[1]}, {Chunk: canonical[0]}}
	result, err := evaluation.EvaluateMetrics(context.Background(), &sequenceSearcher{results: [][]search.Hit{hits, hits}}, "repo-a1", canonical, []evaluation.Case{{
		Category: evaluation.CategoryConcept, Query: "opaque", RelevanceByChunkID: map[string]int{"high": 3, "low": 1},
	}}, evaluation.MetricOptions{})
	if err != nil {
		t.Fatal(err)
	}
	metrics := result.Report.Categories[evaluation.CategoryConcept]
	wantNDCG := (1 + 7/math.Log2(3)) / (7 + 1/math.Log2(3))
	if metrics.QueryCount != 1 || metrics.RecallAt5 != 1 || metrics.RecallAt10 != 1 || metrics.MRRAt10 != 1 ||
		math.Abs(metrics.NDCGAt10-wantNDCG) > 1e-12 || metrics.NoEvidenceAccuracy != 0 {
		t.Fatalf("metrics = %#v, want nDCG %.15f", metrics, wantNDCG)
	}
}

func TestEvaluateMetricsPublishesOnlyNegativeNoEvidenceAccuracy(t *testing.T) {
	canonical := []source.Chunk{metricChunk("a", "one.py", 1)}
	searcher := &sequenceSearcher{results: [][]search.Hit{nil, nil, {{Chunk: canonical[0]}}, nil}}
	result, err := evaluation.EvaluateMetrics(context.Background(), searcher, "repo-a1", canonical, []evaluation.Case{
		{Category: evaluation.CategoryNegativeEvidence, Query: "absent", NoEvidence: true},
		{Category: evaluation.CategoryNegativeEvidence, Query: "present", NoEvidence: true},
	}, evaluation.MetricOptions{})
	if err != nil {
		t.Fatal(err)
	}
	metrics := result.Report.Categories[evaluation.CategoryNegativeEvidence]
	if metrics.Status != evaluation.StatusEvaluated || metrics.QueryCount != 2 || metrics.NoEvidenceAccuracy != 0.5 ||
		metrics.RecallAt5 != 0 || metrics.RecallAt10 != 0 || metrics.MRRAt10 != 0 || metrics.NDCGAt10 != 0 {
		t.Fatalf("negative metrics = %#v", metrics)
	}
}

func metricChunk(id, path string, line int) source.Chunk {
	return source.Chunk{ID: id, Text: "content", Language: source.LanguagePython, SymbolName: "symbol", Reference: source.Reference{Path: path, StartLine: line, EndLine: line}}
}

func scriptedClock(values ...time.Time) func() time.Time {
	index := 0
	return func() time.Time {
		value := values[index]
		index++
		return value
	}
}
