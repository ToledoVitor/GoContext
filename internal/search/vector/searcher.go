package vector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	"github.com/ToledoVitor/GoContext/internal/index"
	"github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/source"
)

var (
	// ErrInvalidSearcher reports missing or typed-nil dependencies.
	ErrInvalidSearcher = errors.New("invalid vector searcher")
	// ErrInvalidQuery reports a query that is unsafe or outside supported bounds.
	ErrInvalidQuery = errors.New("invalid vector search query")
	// ErrBackend reports a sanitized unknown embedder or index failure.
	ErrBackend = errors.New("vector search backend failure")
)

const (
	maxSearcherLimit      = math.MaxInt32
	searcherContextStride = 256
)

// Searcher adapts one query embedding and exact vector index to canonical hits.
type Searcher struct {
	embedder embedding.Embedder
	index    Index
}

// NewSearcher creates a vector searcher from provider-neutral dependencies.
func NewSearcher(embedder embedding.Embedder, index Index) (*Searcher, error) {
	if nilInterface(embedder) || nilInterface(index) {
		return nil, ErrInvalidSearcher
	}
	return &Searcher{embedder: embedder, index: index}, nil
}

// Search validates one repository query before accessing semantic dependencies.
func (s *Searcher) Search(ctx context.Context, query search.Query) ([]search.Hit, error) {
	if ctx == nil {
		return nil, ErrInvalidQuery
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("search vector index: %w", err)
	}
	if strings.TrimSpace(query.RepositoryID) == "" || strings.TrimSpace(query.Text) == "" ||
		query.Limit <= 0 || int64(query.Limit) > maxSearcherLimit {
		return nil, ErrInvalidQuery
	}
	if err := search.ValidateFilter(query.Filter); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidQuery, err)
	}

	metadata, err := s.index.Describe(ctx, query.RepositoryID)
	if err != nil {
		return nil, classifyBackendError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("search vector index: %w", err)
	}
	if strings.TrimSpace(metadata.GenerationID) == "" || strings.TrimSpace(metadata.CorpusRevision) == "" ||
		strings.TrimSpace(metadata.Profile.Fingerprint) == "" || strings.TrimSpace(metadata.Profile.Model) == "" ||
		metadata.Dimensions <= 0 || metadata.Metric != index.VectorMetricCosine {
		return nil, ErrVectorIntegrity
	}

	profile := s.embedder.Profile()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("search vector index: %w", err)
	}
	if profile != metadata.Profile {
		return nil, ErrIncompatibleSpace
	}

	batch, err := s.embedder.Embed(ctx, embedding.PurposeQuery, []string{query.Text})
	if err != nil {
		return nil, classifyEmbeddingError(ctx, err)
	}
	if err := embedding.ValidateBatchContext(ctx, batch, 1); err != nil {
		return nil, classifyEmbeddingError(ctx, err)
	}
	if batch.Profile != metadata.Profile || batch.Dimensions != metadata.Dimensions {
		return nil, ErrInvalidQueryVector
	}
	queryVector, err := cloneVectorContext(ctx, batch.Vectors[0])
	if err != nil {
		return nil, fmt.Errorf("search vector index: %w", err)
	}
	indexQuery := IndexQuery{
		RepositoryID: query.RepositoryID,
		GenerationID: metadata.GenerationID,
		Profile:      metadata.Profile,
		Dimensions:   metadata.Dimensions,
		Metric:       metadata.Metric,
		Vector:       queryVector,
		Filter:       cloneFilter(query.Filter),
		Limit:        query.Limit,
	}
	candidates, err := s.index.Search(ctx, indexQuery)
	if err != nil {
		return nil, classifyBackendError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("search vector index: %w", err)
	}

	hits, err := validatedHits(ctx, candidates)
	if err != nil {
		return nil, err
	}
	if err := sortHitsContext(ctx, hits); err != nil {
		return nil, fmt.Errorf("search vector index: %w", err)
	}
	if len(hits) > query.Limit {
		hits = hits[:query.Limit]
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("search vector index: %w", err)
	}
	return hits, nil
}

func classifyBackendError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return fmt.Errorf("search vector index: %w", contextErr)
	}
	known := []error{
		ErrVectorIntegrity,
		context.Canceled,
		context.DeadlineExceeded,
		index.ErrNotFound,
		index.ErrReindexRequired,
		ErrVectorUnavailable,
		ErrIncompatibleSpace,
		ErrGenerationChanged,
	}
	for _, sentinel := range known {
		if errors.Is(err, sentinel) {
			return fmt.Errorf("search vector index: %w", sentinel)
		}
	}
	return ErrBackend
}

func classifyEmbeddingError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return fmt.Errorf("search vector index: %w", contextErr)
	}
	if errors.Is(err, embedding.ErrInvalidBatch) || errors.Is(err, embedding.ErrInvalidVector) {
		return ErrInvalidQueryVector
	}
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, sentinel) {
			return fmt.Errorf("search vector index: %w", sentinel)
		}
	}
	if errors.Is(err, embedding.ErrSemanticUnavailable) {
		return embedding.ErrSemanticUnavailable
	}
	return ErrBackend
}

func cloneVectorContext(ctx context.Context, values embedding.Vector) (embedding.Vector, error) {
	clone := make(embedding.Vector, len(values))
	for offset := 0; offset < len(values); offset += searcherContextStride {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := offset + searcherContextStride
		if end > len(values) {
			end = len(values)
		}
		copy(clone[offset:end], values[offset:end])
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return clone, nil
}

func cloneFilter(filter search.Filter) search.Filter {
	return search.Filter{
		PathPrefixes: append([]string(nil), filter.PathPrefixes...),
		Languages:    append([]source.Language(nil), filter.Languages...),
	}
}

func validatedHits(ctx context.Context, candidates []Candidate) ([]search.Hit, error) {
	hits := make([]search.Hit, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for position, candidate := range candidates {
		if position%searcherContextStride == 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("search vector index: %w", err)
			}
		}
		chunk := candidate.Chunk
		if chunk.ID == "" || chunk.Text == "" || !chunk.Reference.Valid() {
			return nil, ErrVectorIntegrity
		}
		if _, duplicate := seen[chunk.ID]; duplicate {
			return nil, ErrVectorIntegrity
		}
		seen[chunk.ID] = struct{}{}
		if math.IsNaN(candidate.Similarity) || math.IsInf(candidate.Similarity, 0) || candidate.Similarity < -1 || candidate.Similarity > 1 {
			return nil, ErrVectorIntegrity
		}
		hits = append(hits, search.Hit{Chunk: chunk, Score: (candidate.Similarity + 1) / 2})
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("search vector index: %w", err)
	}
	return hits, nil
}

func sortHitsContext(ctx context.Context, hits []search.Hit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(hits) < 2 {
		return nil
	}
	buffer := make([]search.Hit, len(hits))
	sourceHits := hits
	targetHits := buffer
	inOriginal := true
	for width := 1; ; width *= 2 {
		checks := 0
		for start := 0; start < len(hits); {
			mid := boundedHitIndex(start, width, len(hits))
			end := boundedHitIndex(mid, width, len(hits))
			left, right, output := start, mid, start
			for left < mid || right < end {
				if checks%searcherContextStride == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				checks++
				switch {
				case right >= end || (left < mid && hitBeforeOrEqual(sourceHits[left], sourceHits[right])):
					targetHits[output] = sourceHits[left]
					left++
				default:
					targetHits[output] = sourceHits[right]
					right++
				}
				output++
			}
			start = end
		}
		sourceHits, targetHits = targetHits, sourceHits
		inOriginal = !inOriginal
		if width >= len(hits)-width {
			break
		}
	}
	if !inOriginal {
		for offset := 0; offset < len(hits); offset += searcherContextStride {
			if err := ctx.Err(); err != nil {
				return err
			}
			end := offset + searcherContextStride
			if end > len(hits) {
				end = len(hits)
			}
			copy(hits[offset:end], sourceHits[offset:end])
		}
	}
	return ctx.Err()
}

func hitBeforeOrEqual(left, right search.Hit) bool {
	if left.Score != right.Score {
		return left.Score > right.Score
	}
	leftReference := left.Chunk.Reference
	rightReference := right.Chunk.Reference
	if leftReference.Path != rightReference.Path {
		return leftReference.Path < rightReference.Path
	}
	if leftReference.StartLine != rightReference.StartLine {
		return leftReference.StartLine < rightReference.StartLine
	}
	if leftReference.EndLine != rightReference.EndLine {
		return leftReference.EndLine < rightReference.EndLine
	}
	return left.Chunk.ID <= right.Chunk.ID
}

func boundedHitIndex(start, width, length int) int {
	if width > length-start {
		return length
	}
	return start + width
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

var _ search.Searcher = (*Searcher)(nil)
