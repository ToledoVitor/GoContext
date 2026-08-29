package eval

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/source"
)

var ErrMetrics = errors.New("evaluation metrics failed")

// Case is private evaluation input. Its query and relevant identifiers are
// deliberately excluded from JSON so accidental embedding cannot make them
// query-level output.
type Case struct {
	Category           QueryCategory  `json:"-"`
	Query              string         `json:"-"`
	RelevantChunkIDs   []string       `json:"-"`
	RelevanceByChunkID map[string]int `json:"-"`
	NoEvidence         bool           `json:"-"`
}

type MetricOptions struct {
	Now func() time.Time
}

type MetricAggregate struct {
	Report         RetrievalReport
	QueryP50Micros int64
	QueryP95Micros int64
}

type metricAccumulator struct {
	queries    int
	recall5    float64
	recall10   float64
	mrr10      float64
	ndcg10     float64
	noEvidence float64
}

// EvaluateMetrics runs every suitable private case twice and returns aggregate
// quality, citation, determinism, and latency facts only.
func EvaluateMetrics(
	ctx context.Context,
	searcher search.Searcher,
	repositoryID string,
	corpus []source.Chunk,
	cases []Case,
	options MetricOptions,
) (MetricAggregate, error) {
	report := EmptyReport(repositoryID, DecisionGo).Retrieval
	if err := ctx.Err(); err != nil {
		return MetricAggregate{}, err
	}
	if IsNilDependency(searcher) || !opaqueRepositoryPattern.MatchString(repositoryID) {
		return MetricAggregate{}, ErrMetrics
	}
	canonical, err := canonicalChunkMap(corpus)
	if err != nil {
		return MetricAggregate{}, ErrMetrics
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}

	accumulators := make(map[QueryCategory]*metricAccumulator)
	latencies := make([]time.Duration, 0, len(cases)*2)
	validCitations, citations := 0, 0
	deterministic, evaluatedCases := 0, 0
	for _, evaluationCase := range cases {
		if err := ctx.Err(); err != nil {
			return MetricAggregate{}, err
		}
		if !validQueryCategory(evaluationCase.Category) || strings.TrimSpace(evaluationCase.Query) == "" {
			return MetricAggregate{}, ErrMetrics
		}
		relevant, err := relevantGrades(evaluationCase, canonical)
		if err != nil {
			return MetricAggregate{}, ErrMetrics
		}
		if evaluationCase.NoEvidence {
			if evaluationCase.Category != CategoryNegativeEvidence || len(relevant) != 0 {
				return MetricAggregate{}, ErrMetrics
			}
		} else if evaluationCase.Category == CategoryNegativeEvidence {
			return MetricAggregate{}, ErrMetrics
		} else if len(relevant) == 0 {
			continue
		}

		runs := make([][]search.Hit, 2)
		for run := range runs {
			started := now()
			hits, searchErr := searcher.Search(ctx, search.Query{
				RepositoryID: repositoryID,
				Text:         evaluationCase.Query,
				Limit:        10,
			})
			if searchErr != nil {
				if ctx.Err() != nil {
					return MetricAggregate{}, ctx.Err()
				}
				return MetricAggregate{}, ErrMetrics
			}
			if err := ctx.Err(); err != nil {
				return MetricAggregate{}, err
			}
			finished := now()
			if len(hits) > 10 || finished.Before(started) {
				return MetricAggregate{}, ErrMetrics
			}
			latencies = append(latencies, finished.Sub(started))
			runs[run] = append([]search.Hit(nil), hits...)
			seenHits := make(map[string]struct{}, len(hits))
			for _, hit := range hits {
				_, duplicate := seenHits[hit.Chunk.ID]
				if duplicate {
					return MetricAggregate{}, ErrMetrics
				}
				citations++
				canonicalChunk, present := canonical[hit.Chunk.ID]
				if present && canonicalChunk == hit.Chunk {
					validCitations++
				}
				seenHits[hit.Chunk.ID] = struct{}{}
			}
		}

		accumulator := accumulators[evaluationCase.Category]
		if accumulator == nil {
			accumulator = &metricAccumulator{}
			accumulators[evaluationCase.Category] = accumulator
		}
		if evaluationCase.NoEvidence {
			accumulator.queries++
			if len(runs[0]) == 0 && len(runs[1]) == 0 {
				accumulator.noEvidence++
			}
		} else {
			accumulateQuality(accumulator, runs[0], relevant)
		}
		if sameCompleteOrder(runs[0], runs[1]) {
			deterministic++
		}
		evaluatedCases++
	}

	if err := ctx.Err(); err != nil {
		return MetricAggregate{}, err
	}
	if evaluatedCases == 0 {
		return MetricAggregate{Report: report}, nil
	}
	for category, accumulator := range accumulators {
		count := float64(accumulator.queries)
		metrics := CategoryMetrics{Status: StatusEvaluated, QueryCount: accumulator.queries}
		if category == CategoryNegativeEvidence {
			metrics.NoEvidenceAccuracy = accumulator.noEvidence / count
		} else {
			metrics.RecallAt5 = accumulator.recall5 / count
			metrics.RecallAt10 = accumulator.recall10 / count
			metrics.MRRAt10 = accumulator.mrr10 / count
			metrics.NDCGAt10 = accumulator.ndcg10 / count
		}
		report.Categories[category] = metrics
	}
	report.Status = StatusEvaluated
	if citations == 0 {
		report.CitationValidity = 1
	} else {
		report.CitationValidity = float64(validCitations) / float64(citations)
	}
	report.DeterministicOrderRate = float64(deterministic) / float64(evaluatedCases)
	report.FallbackReason = FallbackLexicalBaseline

	sort.Slice(latencies, func(left, right int) bool { return latencies[left] < latencies[right] })
	if err := ctx.Err(); err != nil {
		return MetricAggregate{}, err
	}
	return MetricAggregate{
		Report:         report,
		QueryP50Micros: percentileMicros(latencies, 0.50),
		QueryP95Micros: percentileMicros(latencies, 0.95),
	}, nil
}

func canonicalChunkMap(chunks []source.Chunk) (map[string]source.Chunk, error) {
	canonical := make(map[string]source.Chunk, len(chunks))
	for _, chunk := range chunks {
		if chunk.ID == "" || chunk.Text == "" || !chunk.Reference.Valid() {
			return nil, ErrMetrics
		}
		if _, duplicate := canonical[chunk.ID]; duplicate {
			return nil, ErrMetrics
		}
		canonical[chunk.ID] = chunk
	}
	return canonical, nil
}

func relevantGrades(evaluationCase Case, corpus map[string]source.Chunk) (map[string]int, error) {
	if len(evaluationCase.RelevantChunkIDs) > 0 && len(evaluationCase.RelevanceByChunkID) > 0 {
		return nil, ErrMetrics
	}
	relevant := make(map[string]int, len(evaluationCase.RelevantChunkIDs)+len(evaluationCase.RelevanceByChunkID))
	for _, id := range evaluationCase.RelevantChunkIDs {
		if _, present := corpus[id]; !present {
			return nil, ErrMetrics
		}
		if _, duplicate := relevant[id]; duplicate {
			return nil, ErrMetrics
		}
		relevant[id] = 1
	}
	for id, relevance := range evaluationCase.RelevanceByChunkID {
		if _, present := corpus[id]; !present || relevance < 1 || relevance > 3 {
			return nil, ErrMetrics
		}
		relevant[id] = relevance
	}
	return relevant, nil
}

func accumulateQuality(accumulator *metricAccumulator, hits []search.Hit, relevant map[string]int) {
	accumulator.queries++
	matched5, matched10 := 0, 0
	dcg := 0.0
	firstRelevant := 0
	for index, hit := range hits {
		relevance, matched := relevant[hit.Chunk.ID]
		if !matched {
			continue
		}
		rank := index + 1
		if rank <= 5 {
			matched5++
		}
		if rank <= 10 {
			matched10++
			dcg += (math.Pow(2, float64(relevance)) - 1) / math.Log2(float64(rank+1))
			if firstRelevant == 0 {
				firstRelevant = rank
			}
		}
	}
	accumulator.recall5 += float64(matched5) / float64(len(relevant))
	accumulator.recall10 += float64(matched10) / float64(len(relevant))
	if firstRelevant > 0 {
		accumulator.mrr10 += 1 / float64(firstRelevant)
	}
	gains := make([]float64, 0, len(relevant))
	for _, relevance := range relevant {
		gains = append(gains, math.Pow(2, float64(relevance))-1)
	}
	sort.Slice(gains, func(left, right int) bool { return gains[left] > gains[right] })
	idealCount := len(gains)
	if idealCount > 10 {
		idealCount = 10
	}
	idcg := 0.0
	for index := 0; index < idealCount; index++ {
		idcg += gains[index] / math.Log2(float64(index+2))
	}
	if idcg > 0 {
		accumulator.ndcg10 += dcg / idcg
	}
}

func sameCompleteOrder(first, second []search.Hit) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index].Chunk.ID != second[index].Chunk.ID {
			return false
		}
	}
	return true
}

func percentileMicros(values []time.Duration, percentile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	index := int(math.Ceil(percentile*float64(len(values)))) - 1
	if index < 0 {
		index = 0
	}
	return values[index].Microseconds()
}

func validQueryCategory(category QueryCategory) bool {
	switch category {
	case CategoryExactSymbol, CategoryConcept, CategoryCrossLayer, CategoryFramework,
		CategoryErrorMessage, CategoryConfigurationPath, CategoryNegativeEvidence:
		return true
	default:
		return false
	}
}
