package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	"github.com/ToledoVitor/GoContext/internal/index"
	"github.com/ToledoVitor/GoContext/internal/search"
	vectorsearch "github.com/ToledoVitor/GoContext/internal/search/vector"
	"github.com/ToledoVitor/GoContext/internal/source"
)

const (
	vectorEncodingVersion  = 1
	maxExactCandidateLimit = math.MaxInt32
	unitNormTolerance      = 1e-4
	vectorContextStride    = 256
)

var errInvalidVectorEncoding = errors.New("invalid vector encoding")

type preparedVector struct {
	chunkID    string
	dimensions int
	valuesBlob []byte
}

func prepareGenerationVectors(generation index.Generation) ([]preparedVector, error) {
	if generation.Metric != index.VectorMetricCosine {
		return nil, index.ErrInvalidGeneration
	}
	if generation.Profile == nil {
		if generation.Dimensions != 0 || len(generation.Vectors) != 0 {
			return nil, index.ErrInvalidGeneration
		}
		return nil, nil
	}
	if strings.TrimSpace(generation.Profile.Fingerprint) == "" ||
		strings.TrimSpace(generation.Profile.Model) == "" ||
		generation.Dimensions <= 0 ||
		len(generation.Vectors) != len(generation.Chunks) {
		return nil, index.ErrInvalidGeneration
	}

	chunkIDs := make(map[string]struct{}, len(generation.Chunks))
	for _, chunk := range generation.Chunks {
		chunkIDs[chunk.ID] = struct{}{}
	}
	records := make(map[string]embedding.Vector, len(generation.Vectors))
	for _, record := range generation.Vectors {
		if _, known := chunkIDs[record.ChunkID]; !known {
			return nil, index.ErrInvalidGeneration
		}
		if _, duplicate := records[record.ChunkID]; duplicate {
			return nil, index.ErrInvalidGeneration
		}
		records[record.ChunkID] = record.Values
	}

	prepared := make([]preparedVector, 0, len(generation.Chunks))
	for _, chunk := range generation.Chunks {
		values, present := records[chunk.ID]
		if !present {
			return nil, index.ErrInvalidGeneration
		}
		normalized, err := normalizeVector(values, generation.Dimensions)
		if err != nil {
			return nil, index.ErrInvalidGeneration
		}
		encoded, err := encodeVector(normalized)
		if err != nil {
			return nil, index.ErrInvalidGeneration
		}
		prepared = append(prepared, preparedVector{
			chunkID:    chunk.ID,
			dimensions: generation.Dimensions,
			valuesBlob: encoded,
		})
	}
	return prepared, nil
}

func normalizeVector(values embedding.Vector, dimensions int) (embedding.Vector, error) {
	return normalizeVectorContext(context.Background(), values, dimensions)
}

func normalizeVectorContext(ctx context.Context, values embedding.Vector, dimensions int) (embedding.Vector, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if dimensions <= 0 || len(values) != dimensions {
		return nil, errInvalidVectorEncoding
	}
	var squaredNorm float64
	for position, value := range values {
		if position%vectorContextStride == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		component := float64(value)
		if math.IsNaN(component) || math.IsInf(component, 0) {
			return nil, errInvalidVectorEncoding
		}
		squaredNorm += component * component
	}
	norm := math.Sqrt(squaredNorm)
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil, errInvalidVectorEncoding
	}
	normalized := make(embedding.Vector, dimensions)
	nonZero := false
	for position, value := range values {
		if position%vectorContextStride == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		normalized[position] = float32(float64(value) / norm)
		nonZero = nonZero || normalized[position] != 0
	}
	if !nonZero {
		return nil, errInvalidVectorEncoding
	}
	return normalized, nil
}

// Describe returns metadata for the reader's pinned vector generation.
func (r *BoundReader) Describe(ctx context.Context, repositoryID string) (vectorsearch.Metadata, error) {
	if err := ctx.Err(); err != nil {
		return vectorsearch.Metadata{}, fmt.Errorf("describe bound vector index: %w", err)
	}
	if r == nil || repositoryID != r.repositoryID {
		return vectorsearch.Metadata{}, vectorsearch.ErrIncompatibleSpace
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return vectorsearch.Metadata{}, fmt.Errorf("describe bound vector index: %w", err)
	}
	if r.closed {
		return vectorsearch.Metadata{}, fmt.Errorf("describe bound vector index: %w", errBoundReaderClosed)
	}
	if r.profileFingerprint == "" && r.profileModel == "" && r.dimensions == 0 {
		return vectorsearch.Metadata{}, r.lexicalOnlyVectorState(ctx, "describe bound vector index")
	}
	return vectorsearch.Metadata{
		GenerationID:   r.generationID,
		CorpusRevision: r.corpusRevision,
		Profile: embedding.Profile{
			Fingerprint: r.profileFingerprint,
			Model:       r.profileModel,
		},
		Dimensions: r.dimensions,
		Metric:     r.metric,
	}, nil
}

// ValidateCorpus verifies the canonical chunks and vector rows for the reader's
// pinned generation before a rollback decision trusts its metadata.
func (r *BoundReader) ValidateCorpus(ctx context.Context) (CorpusMetadata, error) {
	if err := ctx.Err(); err != nil {
		return CorpusMetadata{}, fmt.Errorf("validate bound sqlite corpus: %w", err)
	}
	if r == nil {
		return CorpusMetadata{}, index.ErrReindexRequired
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return CorpusMetadata{}, fmt.Errorf("validate bound sqlite corpus: %w", err)
	}
	if r.closed {
		return CorpusMetadata{}, index.ErrReindexRequired
	}
	chunks, err := r.loadCanonicalChunksLocked(ctx)
	if err != nil {
		return CorpusMetadata{}, err
	}
	if r.profileFingerprint == "" && r.profileModel == "" && r.dimensions == 0 {
		if err := r.validateLexicalOnlyVectorRowsLocked(ctx); err != nil {
			return CorpusMetadata{}, boundCorpusValidationError(ctx, err)
		}
	} else if _, err := r.scanVectorRowsLocked(ctx, chunks, nil, search.Filter{}); err != nil {
		return CorpusMetadata{}, boundCorpusValidationError(ctx, err)
	}
	return CorpusMetadata{
		GenerationID:      r.generationID,
		CorpusRevision:    r.corpusRevision,
		ScanPolicyVersion: r.scanPolicyVersion,
	}, nil
}

func boundCorpusValidationError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return fmt.Errorf("validate bound sqlite corpus: %w", contextErr)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("validate bound sqlite corpus: %w", err)
	}
	return index.ErrReindexRequired
}

// Search scans exact cosine candidates from the reader's pinned generation.
func (r *BoundReader) Search(ctx context.Context, query vectorsearch.IndexQuery) ([]vectorsearch.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("search bound vector index: %w", err)
	}
	if r == nil || query.RepositoryID != r.repositoryID {
		return nil, vectorsearch.ErrIncompatibleSpace
	}
	if query.GenerationID != r.generationID {
		return nil, vectorsearch.ErrGenerationChanged
	}
	lexicalOnly := r.profileFingerprint == "" && r.profileModel == "" && r.dimensions == 0
	if !lexicalOnly && (query.Profile.Fingerprint != r.profileFingerprint || query.Profile.Model != r.profileModel ||
		query.Dimensions != r.dimensions || query.Metric != r.metric) {
		return nil, vectorsearch.ErrIncompatibleSpace
	}
	if err := search.ValidateFilter(query.Filter); err != nil {
		return nil, fmt.Errorf("search bound vector index: %w", err)
	}
	if query.Limit <= 0 || int64(query.Limit) > maxExactCandidateLimit {
		return nil, vectorsearch.ErrInvalidQueryVector
	}
	normalizedQuery, err := normalizeVectorContext(ctx, query.Vector, query.Dimensions)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, fmt.Errorf("search bound vector index: %w", contextErr)
		}
		return nil, vectorsearch.ErrInvalidQueryVector
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("search bound vector index: %w", err)
	}
	if r.closed {
		return nil, fmt.Errorf("search bound vector index: %w", errBoundReaderClosed)
	}
	if lexicalOnly {
		return nil, r.lexicalOnlyVectorState(ctx, "search bound vector index")
	}
	chunks, err := r.loadCanonicalChunksLocked(ctx)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, fmt.Errorf("search bound vector index: %w", contextErr)
		}
		return nil, vectorsearch.ErrVectorIntegrity
	}
	candidates, err := r.scanVectorRowsLocked(ctx, chunks, normalizedQuery, query.Filter)
	if err != nil {
		return nil, err
	}

	if err := sortCandidatesContext(ctx, candidates); err != nil {
		return nil, fmt.Errorf("search bound vector index: %w", err)
	}
	if len(candidates) > query.Limit {
		candidates = candidates[:query.Limit]
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("search bound vector index: %w", err)
	}
	return candidates, nil
}

func (r *BoundReader) scanVectorRowsLocked(
	ctx context.Context,
	chunks []source.Chunk,
	normalizedQuery embedding.Vector,
	filter search.Filter,
) ([]vectorsearch.Candidate, error) {
	canonicalByID := make(map[string]source.Chunk, len(chunks))
	for position, chunk := range chunks {
		if position%vectorContextStride == 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("search bound vector index: %w", err)
			}
		}
		canonicalByID[chunk.ID] = chunk
	}
	rows, err := r.tx.QueryContext(ctx, `
		SELECT chunk_id, encoding_version, dimensions, values_blob
		FROM vectors
		WHERE repository_id = ? AND generation_id = ?
		ORDER BY chunk_id`, r.repositoryID, r.generationID)
	if err != nil {
		return nil, vectorReadError(ctx)
	}

	candidates := make([]vectorsearch.Candidate, 0)
	seen := make(map[string]struct{}, len(chunks))
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("search bound vector index: %w", err)
		}
		var chunkID string
		var version, dimensions sql.NullInt64
		var blob []byte
		if err := rows.Scan(&chunkID, &version, &dimensions, &blob); err != nil {
			_ = rows.Close()
			return nil, vectorReadError(ctx)
		}
		chunk, known := canonicalByID[chunkID]
		if _, duplicate := seen[chunkID]; duplicate || !known || !version.Valid || !dimensions.Valid {
			_ = rows.Close()
			return nil, vectorsearch.ErrVectorIntegrity
		}
		seen[chunkID] = struct{}{}
		if err := validateStoredVectorHeader(version.Int64, dimensions.Int64, r.dimensions); err != nil {
			_ = rows.Close()
			return nil, vectorsearch.ErrVectorIntegrity
		}
		storedVector, err := decodeVectorContext(ctx, vectorEncodingVersion, r.dimensions, blob)
		if err != nil {
			_ = rows.Close()
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, fmt.Errorf("search bound vector index: %w", contextErr)
			}
			return nil, vectorsearch.ErrVectorIntegrity
		}
		unitNorm, err := hasUnitNormContext(ctx, storedVector)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		if !unitNorm {
			_ = rows.Close()
			return nil, vectorsearch.ErrVectorIntegrity
		}
		if normalizedQuery != nil {
			similarity, err := cosineSimilarity(ctx, normalizedQuery, storedVector)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			matches, err := vectorMatchesFilterContext(ctx, chunk, filter)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			if matches {
				candidates = append(candidates, vectorsearch.Candidate{Chunk: chunk, Similarity: similarity})
			}
		}
	}
	iterationErr := rows.Err()
	closeErr := rows.Close()
	if iterationErr != nil || closeErr != nil {
		return nil, vectorReadError(ctx)
	}
	if len(seen) != len(chunks) {
		return nil, vectorsearch.ErrVectorIntegrity
	}
	return candidates, nil
}

func (r *BoundReader) lexicalOnlyVectorState(ctx context.Context, operation string) error {
	if err := r.validateLexicalOnlyVectorRowsLocked(ctx); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return fmt.Errorf("%s: %w", operation, contextErr)
		}
		return err
	}
	return vectorsearch.ErrVectorUnavailable
}

func (r *BoundReader) validateLexicalOnlyVectorRowsLocked(ctx context.Context) error {
	var vectorCount int64
	if err := r.tx.QueryRowContext(ctx, `
		SELECT count(*)
		FROM vectors
		WHERE repository_id = ? AND generation_id = ?`,
		r.repositoryID, r.generationID,
	).Scan(&vectorCount); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return vectorsearch.ErrVectorIntegrity
	}
	if vectorCount != 0 {
		return vectorsearch.ErrVectorIntegrity
	}
	return nil
}

func vectorMatchesFilterContext(ctx context.Context, chunk source.Chunk, filter search.Filter) (bool, error) {
	if len(filter.PathPrefixes) > 0 {
		pathMatched := false
		for position, prefix := range filter.PathPrefixes {
			if position%vectorContextStride == 0 {
				if err := ctx.Err(); err != nil {
					return false, fmt.Errorf("search bound vector index: %w", err)
				}
			}
			normalized := strings.TrimSuffix(prefix, "/")
			if chunk.Reference.Path == normalized || strings.HasPrefix(chunk.Reference.Path, normalized+"/") {
				pathMatched = true
				break
			}
		}
		if !pathMatched {
			return false, nil
		}
	}
	if len(filter.Languages) == 0 {
		return true, nil
	}
	for position, language := range filter.Languages {
		if position%vectorContextStride == 0 {
			if err := ctx.Err(); err != nil {
				return false, fmt.Errorf("search bound vector index: %w", err)
			}
		}
		if chunk.Language == language {
			return true, nil
		}
	}
	return false, nil
}

func sortCandidatesContext(ctx context.Context, candidates []vectorsearch.Candidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(candidates) < 2 {
		return nil
	}
	buffer := make([]vectorsearch.Candidate, len(candidates))
	sourceCandidates := candidates
	targetCandidates := buffer
	inOriginal := true
	for width := 1; ; width *= 2 {
		checks := 0
		for start := 0; start < len(candidates); {
			mid := boundedVectorIndex(start, width, len(candidates))
			end := boundedVectorIndex(mid, width, len(candidates))
			left, right, output := start, mid, start
			for left < mid || right < end {
				if checks%vectorContextStride == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				checks++
				switch {
				case right >= end || (left < mid && candidateBeforeOrEqual(sourceCandidates[left], sourceCandidates[right])):
					targetCandidates[output] = sourceCandidates[left]
					left++
				default:
					targetCandidates[output] = sourceCandidates[right]
					right++
				}
				output++
			}
			start = end
		}
		sourceCandidates, targetCandidates = targetCandidates, sourceCandidates
		inOriginal = !inOriginal
		if width >= len(candidates)-width {
			break
		}
	}
	if !inOriginal {
		for offset := 0; offset < len(candidates); offset += vectorContextStride {
			if err := ctx.Err(); err != nil {
				return err
			}
			end := offset + vectorContextStride
			if end > len(candidates) {
				end = len(candidates)
			}
			copy(candidates[offset:end], sourceCandidates[offset:end])
		}
	}
	return ctx.Err()
}

func candidateBeforeOrEqual(left, right vectorsearch.Candidate) bool {
	if left.Similarity != right.Similarity {
		return left.Similarity > right.Similarity
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

func boundedVectorIndex(start, width, length int) int {
	if width > length-start {
		return length
	}
	return start + width
}

func hasUnitNormContext(ctx context.Context, values embedding.Vector) (bool, error) {
	var squaredNorm float64
	for position, value := range values {
		if position%vectorContextStride == 0 {
			if err := ctx.Err(); err != nil {
				return false, fmt.Errorf("search bound vector index: %w", err)
			}
		}
		component := float64(value)
		squaredNorm += component * component
	}
	return math.Abs(math.Sqrt(squaredNorm)-1) <= unitNormTolerance, nil
}

func cosineSimilarity(ctx context.Context, left, right embedding.Vector) (float64, error) {
	if len(left) != len(right) || len(left) == 0 {
		return 0, vectorsearch.ErrVectorIntegrity
	}
	var dot, leftSquaredNorm, rightSquaredNorm float64
	for position := range left {
		if position%vectorContextStride == 0 {
			if err := ctx.Err(); err != nil {
				return 0, fmt.Errorf("search bound vector index: %w", err)
			}
		}
		leftComponent := float64(left[position])
		rightComponent := float64(right[position])
		dot += leftComponent * rightComponent
		leftSquaredNorm += leftComponent * leftComponent
		rightSquaredNorm += rightComponent * rightComponent
	}
	denominator := math.Sqrt(leftSquaredNorm * rightSquaredNorm)
	if denominator == 0 || math.IsNaN(denominator) || math.IsInf(denominator, 0) {
		return 0, vectorsearch.ErrVectorIntegrity
	}
	similarity := dot / denominator
	if similarity > 1 && similarity <= 1+1e-12 {
		similarity = 1
	}
	if similarity < -1 && similarity >= -1-1e-12 {
		similarity = -1
	}
	if math.IsNaN(similarity) || math.IsInf(similarity, 0) || similarity < -1 || similarity > 1 {
		return 0, vectorsearch.ErrVectorIntegrity
	}
	return similarity, nil
}

func vectorReadError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("search bound vector index: %w", err)
	}
	return vectorsearch.ErrVectorIntegrity
}

var _ vectorsearch.Index = (*BoundReader)(nil)

func encodeVector(values embedding.Vector) ([]byte, error) {
	if err := validateVectorComponents(values, len(values)); err != nil {
		return nil, errInvalidVectorEncoding
	}
	encoded := make([]byte, len(values)*4)
	for position, value := range values {
		binary.LittleEndian.PutUint32(encoded[position*4:], math.Float32bits(value))
	}
	return encoded, nil
}

func decodeVector(encodingVersion, dimensions int, encoded []byte) (embedding.Vector, error) {
	return decodeVectorContext(context.Background(), encodingVersion, dimensions, encoded)
}

func validateStoredVectorHeader(encodingVersion, dimensions int64, expectedDimensions int) error {
	if encodingVersion != int64(vectorEncodingVersion) || expectedDimensions <= 0 || dimensions != int64(expectedDimensions) {
		return vectorsearch.ErrVectorIntegrity
	}
	return nil
}

func decodeVectorContext(ctx context.Context, encodingVersion, dimensions int, encoded []byte) (embedding.Vector, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if encodingVersion != vectorEncodingVersion || dimensions <= 0 || len(encoded) == 0 || len(encoded)%4 != 0 || len(encoded)/4 != dimensions {
		return nil, vectorsearch.ErrVectorIntegrity
	}
	values := make(embedding.Vector, dimensions)
	for position := range values {
		if position%vectorContextStride == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		values[position] = math.Float32frombits(binary.LittleEndian.Uint32(encoded[position*4:]))
	}
	if err := validateVectorComponentsContext(ctx, values, dimensions); err != nil {
		return nil, vectorsearch.ErrVectorIntegrity
	}
	return values, nil
}

func validateVectorComponents(values embedding.Vector, dimensions int) error {
	return validateVectorComponentsContext(context.Background(), values, dimensions)
}

func validateVectorComponentsContext(ctx context.Context, values embedding.Vector, dimensions int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dimensions <= 0 || len(values) != dimensions {
		return errInvalidVectorEncoding
	}
	nonZero := false
	for position, value := range values {
		if position%vectorContextStride == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		component := float64(value)
		if math.IsNaN(component) || math.IsInf(component, 0) {
			return errInvalidVectorEncoding
		}
		nonZero = nonZero || value != 0
	}
	if !nonZero {
		return errInvalidVectorEncoding
	}
	return nil
}
