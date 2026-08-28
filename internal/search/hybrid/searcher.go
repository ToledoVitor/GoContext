// Package hybrid composes mandatory lexical evidence with optional vector evidence.
package hybrid

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	"github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/search/vector"
)

var (
	// ErrInvalidSearcher reports a missing mandatory dependency.
	ErrInvalidSearcher = errors.New("invalid hybrid searcher")
	// ErrInvalidConfig reports unsafe or unsupported hybrid policy.
	ErrInvalidConfig = errors.New("invalid hybrid search config")
	// ErrInvalidQuery reports a query that is unsafe or outside supported bounds.
	ErrInvalidQuery = errors.New("invalid hybrid search query")
	// ErrLexicalFailure reports a sanitized mandatory-backend failure.
	ErrLexicalFailure = errors.New("hybrid lexical search failure")
	// ErrVectorFailure reports a sanitized non-degradable vector-backend failure.
	ErrVectorFailure = errors.New("hybrid vector search failure")
	// ErrEvidenceIntegrity reports malformed or conflicting backend evidence.
	ErrEvidenceIntegrity = errors.New("hybrid evidence integrity failure")
)

const (
	defaultRRFK                = 60
	defaultWeight              = 1.0
	defaultCandidateMultiplier = 4
	defaultSemanticTimeout     = 3 * time.Second
	maxSemanticTimeout         = 5 * time.Minute
	maxQueryLimit              = 200
	contextCheckStride         = 256
)

// SemanticMode controls read-side semantic retrieval.
type SemanticMode string

const (
	SemanticOff       SemanticMode = "off"
	SemanticPreferred SemanticMode = "preferred"
	SemanticRequired  SemanticMode = "required"

	eventBackendVector       = "vector"
	eventKindFallback        = "fallback"
	eventKindFailure         = "failure"
	eventVectorUnavailable   = "vector_unavailable"
	eventTimeout             = "timeout"
	eventSemanticUnavailable = "semantic_unavailable"
	eventIncompatibleSpace   = "incompatible_space"
	eventGenerationChanged   = "generation_changed"
)

// Event is a sanitized observation about the optional vector backend.
type Event struct {
	Backend string
	Kind    string
	Reason  string
	Latency time.Duration
}

// Observer receives sanitized hybrid-search events. Calls made by one Searcher
// are serialized. Implementations must honor ctx and return promptly: Search
// cannot forcibly stop an arbitrary callback that ignores cancellation. A
// blocked callback delays later events, while a canceled search can stop waiting
// for its turn without starting another goroutine. Implementations must not
// synchronously re-enter the same Searcher from Observe.
type Observer interface {
	Observe(context.Context, Event)
}

// Config controls rank fusion and optional vector behavior.
type Config struct {
	Mode                SemanticMode
	RRFK                int
	LexicalWeight       float64
	VectorWeight        float64
	CandidateMultiplier int
	SemanticTimeout     time.Duration
}

// Searcher combines lexical and vector searchers without exposing provider details.
type Searcher struct {
	lexical             search.Searcher
	vector              search.Searcher
	observer            Observer
	observerGate        chan struct{}
	mode                SemanticMode
	rrfK                int
	lexicalWeight       float64
	vectorWeight        float64
	candidateMultiplier int
	semanticTimeout     time.Duration
	normalizer          float64
}

// NewSearcher validates dependencies and applies safe hybrid defaults.
func NewSearcher(lexical search.Searcher, vector search.Searcher, observer Observer, config Config) (*Searcher, error) {
	if nilInterface(lexical) {
		return nil, ErrInvalidSearcher
	}
	if nilInterface(vector) {
		vector = nil
	}
	if nilInterface(observer) {
		observer = nil
	}
	var observerGate chan struct{}
	if observer != nil {
		observerGate = make(chan struct{}, 1)
	}

	if config.Mode == "" {
		config.Mode = SemanticOff
	}
	if config.Mode != SemanticOff && config.Mode != SemanticPreferred && config.Mode != SemanticRequired {
		return nil, ErrInvalidConfig
	}
	if config.RRFK == 0 {
		config.RRFK = defaultRRFK
	}
	if config.RRFK < 0 {
		return nil, ErrInvalidConfig
	}
	if config.LexicalWeight == 0 {
		config.LexicalWeight = defaultWeight
	}
	if !finitePositive(config.LexicalWeight) {
		return nil, ErrInvalidConfig
	}
	if config.VectorWeight == 0 {
		config.VectorWeight = defaultWeight
	}
	if !finitePositive(config.VectorWeight) {
		return nil, ErrInvalidConfig
	}
	if config.CandidateMultiplier == 0 {
		config.CandidateMultiplier = defaultCandidateMultiplier
	}
	if config.CandidateMultiplier < 0 {
		return nil, ErrInvalidConfig
	}
	if config.SemanticTimeout == 0 {
		config.SemanticTimeout = defaultSemanticTimeout
	}
	if config.SemanticTimeout < 0 || config.SemanticTimeout > maxSemanticTimeout {
		return nil, ErrInvalidConfig
	}
	normalizer := config.LexicalWeight/(float64(config.RRFK)+1) + config.VectorWeight/(float64(config.RRFK)+1)
	if !finitePositive(normalizer) {
		return nil, ErrInvalidConfig
	}
	worstRankDenominator := float64(config.RRFK) + maxQueryLimit
	lexicalWorstContribution := config.LexicalWeight / worstRankDenominator
	vectorWorstContribution := config.VectorWeight / worstRankDenominator
	if !finitePositive(lexicalWorstContribution) ||
		!finitePositive(vectorWorstContribution) ||
		!finitePositive(lexicalWorstContribution/normalizer) ||
		!finitePositive(vectorWorstContribution/normalizer) {
		return nil, ErrInvalidConfig
	}

	return &Searcher{
		lexical:             lexical,
		vector:              vector,
		observer:            observer,
		observerGate:        observerGate,
		mode:                config.Mode,
		rrfK:                config.RRFK,
		lexicalWeight:       config.LexicalWeight,
		vectorWeight:        config.VectorWeight,
		candidateMultiplier: config.CandidateMultiplier,
		semanticTimeout:     config.SemanticTimeout,
		normalizer:          normalizer,
	}, nil
}

// Search retrieves repository evidence.
func (s *Searcher) Search(ctx context.Context, query search.Query) ([]search.Hit, error) {
	if err := validateQuery(ctx, query); err != nil {
		return nil, err
	}
	if s.mode == SemanticOff {
		return s.searchLexicalOnly(ctx, query)
	}
	if s.vector == nil {
		if s.mode == SemanticRequired {
			if err := s.observe(ctx, eventKindFailure, eventVectorUnavailable, 0); err != nil {
				return nil, err
			}
			return nil, vector.ErrVectorUnavailable
		}
		hits, err := s.searchLexicalFallback(ctx, query)
		if err != nil {
			return nil, err
		}
		if err := s.observe(ctx, eventKindFallback, eventVectorUnavailable, 0); err != nil {
			return nil, err
		}
		return hits, nil
	}
	return s.searchHybrid(ctx, query)
}

func (s *Searcher) searchLexicalFallback(ctx context.Context, query search.Query) ([]search.Hit, error) {
	hits, err := s.searchLexicalOnly(ctx, query)
	if err != nil {
		return nil, err
	}
	if err := validateHits(ctx, hits); err != nil {
		return nil, err
	}
	return lexicalTop(hits, query.Limit), nil
}

type backendResult struct {
	hits       []search.Hit
	err        error
	contextErr error
	latency    time.Duration
}

func (s *Searcher) searchHybrid(ctx context.Context, query search.Query) ([]search.Hit, error) {
	backendQuery := cloneQuery(query)
	backendQuery.Limit = candidateLimit(query.Limit, s.candidateMultiplier)
	lexicalQuery := cloneQuery(backendQuery)
	vectorQuery := cloneQuery(backendQuery)

	semanticCtx, cancelSemantic := context.WithTimeout(ctx, s.semanticTimeout)
	defer cancelSemantic()
	lexicalResults := make(chan backendResult, 1)
	vectorResults := make(chan backendResult, 1)
	vectorStarted := time.Now()

	go func() {
		hits, err := s.lexical.Search(ctx, lexicalQuery)
		lexicalResults <- backendResult{hits: hits, err: err}
	}()
	go func() {
		hits, err := s.vector.Search(semanticCtx, vectorQuery)
		semanticErr := semanticCtx.Err()
		if err == nil && semanticErr != nil {
			err = semanticErr
			hits = nil
		}
		vectorResults <- backendResult{
			hits:       hits,
			err:        err,
			contextErr: semanticErr,
			latency:    time.Since(vectorStarted),
		}
	}()

	var lexicalResult, vectorResult backendResult
	lexicalDone := false
	vectorDone := false
	lexicalResultChannel := lexicalResults
	vectorResultChannel := vectorResults
	semanticDone := semanticCtx.Done()
	for !lexicalDone || !vectorDone {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("search hybrid evidence: %w", ctx.Err())
		case lexicalResult = <-lexicalResultChannel:
			lexicalDone = true
			lexicalResultChannel = nil
		case vectorResult = <-vectorResultChannel:
			vectorDone = true
			vectorResultChannel = nil
			semanticDone = nil
		case <-semanticDone:
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("search hybrid evidence: %w", err)
			}
			vectorResult = backendResult{
				err:        semanticCtx.Err(),
				contextErr: semanticCtx.Err(),
				latency:    time.Since(vectorStarted),
			}
			vectorDone = true
			vectorResultChannel = nil
			semanticDone = nil
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("search hybrid evidence: %w", err)
	}
	if lexicalResult.err != nil {
		return nil, classifyLexicalError(ctx, lexicalResult.err)
	}
	if err := validateHits(ctx, lexicalResult.hits); err != nil {
		return nil, err
	}
	if vectorResult.err != nil {
		if cause, reason, degradable := degradableVectorError(vectorResult); degradable {
			if s.mode == SemanticRequired {
				if err := s.observe(ctx, eventKindFailure, reason, vectorResult.latency); err != nil {
					return nil, err
				}
				return nil, cause
			}
			if err := s.observe(ctx, eventKindFallback, reason, vectorResult.latency); err != nil {
				return nil, err
			}
			return lexicalTop(lexicalResult.hits, query.Limit), nil
		}
		return nil, classifyFatalVectorError(vectorResult.err)
	}
	if err := validateHits(ctx, vectorResult.hits); err != nil {
		return nil, err
	}
	return s.fuse(ctx, lexicalResult.hits, vectorResult.hits, query.Limit)
}

func classifyFatalVectorError(err error) error {
	if errors.Is(err, vector.ErrVectorIntegrity) {
		return errors.Join(ErrEvidenceIntegrity, vector.ErrVectorIntegrity)
	}
	for _, sentinel := range []error{vector.ErrInvalidQueryVector, vector.ErrBackend} {
		if errors.Is(err, sentinel) {
			return errors.Join(ErrVectorFailure, sentinel)
		}
	}
	return ErrVectorFailure
}

func degradableVectorError(result backendResult) (error, string, bool) {
	if errors.Is(result.err, vector.ErrVectorIntegrity) || errors.Is(result.err, vector.ErrInvalidQueryVector) {
		return nil, "", false
	}
	classifications := []struct {
		sentinel error
		reason   string
	}{
		{sentinel: embedding.ErrSemanticUnavailable, reason: eventSemanticUnavailable},
		{sentinel: vector.ErrVectorUnavailable, reason: eventVectorUnavailable},
		{sentinel: vector.ErrIncompatibleSpace, reason: eventIncompatibleSpace},
		{sentinel: vector.ErrGenerationChanged, reason: eventGenerationChanged},
	}
	for _, classification := range classifications {
		if errors.Is(result.err, classification.sentinel) {
			return classification.sentinel, classification.reason, true
		}
	}
	if errors.Is(result.contextErr, context.DeadlineExceeded) {
		return context.DeadlineExceeded, eventTimeout, true
	}
	return nil, "", false
}

func lexicalTop(hits []search.Hit, limit int) []search.Hit {
	if len(hits) > limit {
		return hits[:limit]
	}
	return hits
}

type fusedHit struct {
	hit          search.Hit
	rawScore     float64
	contributors int
	bestRank     int
}

func validateHits(ctx context.Context, hits []search.Hit) error {
	seen := make(map[string]struct{}, len(hits))
	for position, hit := range hits {
		if position%contextCheckStride == 0 {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("search hybrid evidence: %w", err)
			}
		}
		if hit.Chunk.ID == "" || hit.Chunk.Text == "" || !hit.Chunk.Reference.Valid() ||
			math.IsNaN(hit.Score) || math.IsInf(hit.Score, 0) {
			return ErrEvidenceIntegrity
		}
		if _, duplicate := seen[hit.Chunk.ID]; duplicate {
			return ErrEvidenceIntegrity
		}
		seen[hit.Chunk.ID] = struct{}{}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("search hybrid evidence: %w", err)
	}
	return nil
}

func (s *Searcher) fuse(ctx context.Context, lexicalHits, vectorHits []search.Hit, limit int) ([]search.Hit, error) {
	fusedByID := make(map[string]*fusedHit, len(lexicalHits)+len(vectorHits))
	if err := s.addRanks(ctx, fusedByID, lexicalHits, s.lexicalWeight); err != nil {
		return nil, err
	}
	if err := s.addRanks(ctx, fusedByID, vectorHits, s.vectorWeight); err != nil {
		return nil, err
	}

	fused := make([]fusedHit, 0, len(fusedByID))
	position := 0
	for _, candidate := range fusedByID {
		if position%contextCheckStride == 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("search hybrid evidence: %w", err)
			}
		}
		candidate.hit.Score = candidate.rawScore / s.normalizer
		if !finitePositive(candidate.hit.Score) || candidate.hit.Score > 1 {
			return nil, ErrEvidenceIntegrity
		}
		fused = append(fused, *candidate)
		position++
	}
	if err := sortFusedContext(ctx, fused); err != nil {
		return nil, fmt.Errorf("search hybrid evidence: %w", err)
	}
	if limit > len(fused) {
		limit = len(fused)
	}
	hits := make([]search.Hit, limit)
	for position := range hits {
		if position%contextCheckStride == 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("search hybrid evidence: %w", err)
			}
		}
		hits[position] = fused[position].hit
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("search hybrid evidence: %w", err)
	}
	return hits, nil
}

func (s *Searcher) addRanks(ctx context.Context, fusedByID map[string]*fusedHit, hits []search.Hit, weight float64) error {
	for position, hit := range hits {
		if position%contextCheckStride == 0 {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("search hybrid evidence: %w", err)
			}
		}
		rank := position + 1
		contribution := weight / (float64(s.rrfK) + float64(rank))
		candidate, exists := fusedByID[hit.Chunk.ID]
		if !exists {
			fusedByID[hit.Chunk.ID] = &fusedHit{
				hit:          search.Hit{Chunk: hit.Chunk},
				rawScore:     contribution,
				contributors: 1,
				bestRank:     rank,
			}
			continue
		}
		if candidate.hit.Chunk != hit.Chunk {
			return ErrEvidenceIntegrity
		}
		candidate.rawScore += contribution
		candidate.contributors++
		if rank < candidate.bestRank {
			candidate.bestRank = rank
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("search hybrid evidence: %w", err)
	}
	return nil
}

func sortFusedContext(ctx context.Context, hits []fusedHit) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(hits) < 2 {
		return nil
	}
	buffer := make([]fusedHit, len(hits))
	sourceHits := hits
	targetHits := buffer
	inOriginal := true
	for width := 1; ; width *= 2 {
		checks := 0
		for start := 0; start < len(hits); {
			mid := boundedIndex(start, width, len(hits))
			end := boundedIndex(mid, width, len(hits))
			left, right, output := start, mid, start
			for left < mid || right < end {
				if checks%contextCheckStride == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				checks++
				if right >= end || (left < mid && fusedBeforeOrEqual(sourceHits[left], sourceHits[right])) {
					targetHits[output] = sourceHits[left]
					left++
				} else {
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
		for offset := 0; offset < len(hits); offset += contextCheckStride {
			if err := ctx.Err(); err != nil {
				return err
			}
			end := offset + contextCheckStride
			if end > len(hits) {
				end = len(hits)
			}
			copy(hits[offset:end], sourceHits[offset:end])
		}
	}
	return ctx.Err()
}

func fusedBeforeOrEqual(left, right fusedHit) bool {
	if left.hit.Score != right.hit.Score {
		return left.hit.Score > right.hit.Score
	}
	if left.contributors != right.contributors {
		return left.contributors > right.contributors
	}
	if left.bestRank != right.bestRank {
		return left.bestRank < right.bestRank
	}
	leftReference := left.hit.Chunk.Reference
	rightReference := right.hit.Chunk.Reference
	if leftReference.Path != rightReference.Path {
		return leftReference.Path < rightReference.Path
	}
	if leftReference.StartLine != rightReference.StartLine {
		return leftReference.StartLine < rightReference.StartLine
	}
	if leftReference.EndLine != rightReference.EndLine {
		return leftReference.EndLine < rightReference.EndLine
	}
	return left.hit.Chunk.ID <= right.hit.Chunk.ID
}

func boundedIndex(start, width, length int) int {
	if width > length-start {
		return length
	}
	return start + width
}

func candidateLimit(limit, multiplier int) int {
	if multiplier > maxQueryLimit/limit {
		return maxQueryLimit
	}
	candidate := limit * multiplier
	if candidate < 20 {
		return 20
	}
	if candidate > maxQueryLimit {
		return maxQueryLimit
	}
	return candidate
}

func (s *Searcher) observe(ctx context.Context, kind, reason string, latency time.Duration) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("search hybrid evidence: %w", err)
	}
	if latency < 0 {
		latency = 0
	}
	if s.observer != nil {
		select {
		case s.observerGate <- struct{}{}:
		case <-ctx.Done():
			return fmt.Errorf("search hybrid evidence: %w", ctx.Err())
		}
		defer func() { <-s.observerGate }()
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("search hybrid evidence: %w", err)
		}
		func() {
			defer func() {
				_ = recover()
			}()
			s.observer.Observe(ctx, Event{Backend: eventBackendVector, Kind: kind, Reason: reason, Latency: latency})
		}()
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("search hybrid evidence: %w", err)
	}
	return nil
}

func (s *Searcher) searchLexicalOnly(ctx context.Context, query search.Query) ([]search.Hit, error) {
	hits, err := s.lexical.Search(ctx, cloneQuery(query))
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, fmt.Errorf("search hybrid evidence: %w", contextErr)
	}
	if err != nil {
		return nil, classifyLexicalError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("search hybrid evidence: %w", err)
	}
	return hits, nil
}

func classifyLexicalError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return fmt.Errorf("search hybrid evidence: %w", contextErr)
	}
	if errors.Is(err, search.ErrInvalidFilter) {
		return fmt.Errorf("%w: %w", ErrInvalidQuery, search.ErrInvalidFilter)
	}
	return ErrLexicalFailure
}

func validateQuery(ctx context.Context, query search.Query) error {
	if ctx == nil {
		return ErrInvalidQuery
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("search hybrid evidence: %w", err)
	}
	if strings.TrimSpace(query.RepositoryID) == "" || strings.TrimSpace(query.Text) == "" ||
		query.Limit <= 0 || query.Limit > maxQueryLimit {
		return ErrInvalidQuery
	}
	if err := search.ValidateFilter(query.Filter); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidQuery, search.ErrInvalidFilter)
	}
	return nil
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func cloneQuery(query search.Query) search.Query {
	query.Filter.PathPrefixes = cloneSlice(query.Filter.PathPrefixes)
	query.Filter.Languages = cloneSlice(query.Filter.Languages)
	return query
}

func cloneSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	cloned := make([]T, len(values))
	copy(cloned, values)
	return cloned
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
