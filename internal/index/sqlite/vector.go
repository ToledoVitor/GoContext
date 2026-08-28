package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
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
	if err := validateVectorComponents(values, dimensions); err != nil {
		return nil, err
	}
	var squaredNorm float64
	for _, value := range values {
		component := float64(value)
		squaredNorm += component * component
	}
	norm := math.Sqrt(squaredNorm)
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil, errInvalidVectorEncoding
	}
	normalized := make(embedding.Vector, dimensions)
	for position, value := range values {
		normalized[position] = float32(float64(value) / norm)
	}
	if err := validateVectorComponents(normalized, dimensions); err != nil {
		return nil, err
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
	if r.closed {
		return vectorsearch.Metadata{}, fmt.Errorf("describe bound vector index: %w", errBoundReaderClosed)
	}
	if r.profileFingerprint == "" && r.profileModel == "" && r.dimensions == 0 {
		return vectorsearch.Metadata{}, vectorsearch.ErrVectorUnavailable
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

// Search scans exact cosine candidates from the reader's pinned generation.
func (r *BoundReader) Search(ctx context.Context, query vectorsearch.IndexQuery) ([]vectorsearch.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("search bound vector index: %w", err)
	}
	if err := search.ValidateFilter(query.Filter); err != nil {
		return nil, fmt.Errorf("search bound vector index: %w", err)
	}
	if query.Limit <= 0 || int64(query.Limit) > maxExactCandidateLimit {
		return nil, vectorsearch.ErrInvalidQueryVector
	}
	normalizedQuery, err := normalizeVector(query.Vector, query.Dimensions)
	if err != nil {
		return nil, vectorsearch.ErrInvalidQueryVector
	}
	if r == nil {
		return nil, vectorsearch.ErrIncompatibleSpace
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, fmt.Errorf("search bound vector index: %w", errBoundReaderClosed)
	}
	if r.profileFingerprint == "" && r.profileModel == "" && r.dimensions == 0 {
		return nil, vectorsearch.ErrVectorUnavailable
	}
	if query.RepositoryID != r.repositoryID || query.GenerationID != r.generationID ||
		query.Profile.Fingerprint != r.profileFingerprint || query.Profile.Model != r.profileModel ||
		query.Dimensions != r.dimensions || query.Metric != r.metric {
		return nil, vectorsearch.ErrIncompatibleSpace
	}

	rows, err := r.tx.QueryContext(ctx, `
		SELECT c.chunk_id, c.text, c.language, c.symbol_name, c.path, c.start_line, c.end_line,
		       v.encoding_version, v.dimensions, v.values_blob
		FROM chunks AS c
		LEFT JOIN vectors AS v
		  ON v.repository_id = c.repository_id
		 AND v.generation_id = c.generation_id
		 AND v.chunk_id = c.chunk_id
		WHERE c.repository_id = ? AND c.generation_id = ?
		ORDER BY c.ordinal`, r.repositoryID, r.generationID)
	if err != nil {
		return nil, vectorReadError(ctx)
	}

	chunks := make([]source.Chunk, 0)
	candidates := make([]vectorsearch.Candidate, 0)
	seen := make(map[string]struct{})
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("search bound vector index: %w", err)
		}
		var chunk source.Chunk
		var language string
		var version, dimensions sql.NullInt64
		var blob []byte
		if err := rows.Scan(
			&chunk.ID, &chunk.Text, &language, &chunk.SymbolName,
			&chunk.Reference.Path, &chunk.Reference.StartLine, &chunk.Reference.EndLine,
			&version, &dimensions, &blob,
		); err != nil {
			_ = rows.Close()
			return nil, vectorReadError(ctx)
		}
		if _, duplicate := seen[chunk.ID]; duplicate || !version.Valid || !dimensions.Valid {
			_ = rows.Close()
			return nil, vectorsearch.ErrVectorIntegrity
		}
		seen[chunk.ID] = struct{}{}
		chunk.Language = source.Language(language)
		storedVector, err := decodeVector(int(version.Int64), int(dimensions.Int64), blob)
		if err != nil || int(dimensions.Int64) != r.dimensions || !hasUnitNorm(storedVector) {
			_ = rows.Close()
			return nil, vectorsearch.ErrVectorIntegrity
		}
		similarity, err := cosineSimilarity(ctx, normalizedQuery, storedVector)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		chunks = append(chunks, chunk)
		if search.MatchesFilter(chunk, query.Filter) {
			candidates = append(candidates, vectorsearch.Candidate{Chunk: chunk, Similarity: similarity})
		}
	}
	iterationErr := rows.Err()
	closeErr := rows.Close()
	if iterationErr != nil || closeErr != nil {
		return nil, vectorReadError(ctx)
	}

	corpus, err := source.NewCorpus(r.scanPolicyVersion, chunks)
	if err != nil || corpus.Revision != r.corpusRevision || canonicalContentDigest(chunks) != r.contentDigest {
		return nil, vectorsearch.ErrVectorIntegrity
	}
	var orphaned int
	if err := r.tx.QueryRowContext(ctx, `
		SELECT count(*)
		FROM vectors AS v
		LEFT JOIN chunks AS c
		  ON c.repository_id = v.repository_id
		 AND c.generation_id = v.generation_id
		 AND c.chunk_id = v.chunk_id
		WHERE v.repository_id = ? AND v.generation_id = ? AND c.chunk_id IS NULL`,
		r.repositoryID, r.generationID,
	).Scan(&orphaned); err != nil {
		return nil, vectorReadError(ctx)
	}
	if orphaned != 0 {
		return nil, vectorsearch.ErrVectorIntegrity
	}

	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].Similarity != candidates[right].Similarity {
			return candidates[left].Similarity > candidates[right].Similarity
		}
		leftReference := candidates[left].Chunk.Reference
		rightReference := candidates[right].Chunk.Reference
		if leftReference.Path != rightReference.Path {
			return leftReference.Path < rightReference.Path
		}
		if leftReference.StartLine != rightReference.StartLine {
			return leftReference.StartLine < rightReference.StartLine
		}
		if leftReference.EndLine != rightReference.EndLine {
			return leftReference.EndLine < rightReference.EndLine
		}
		return candidates[left].Chunk.ID < candidates[right].Chunk.ID
	})
	if len(candidates) > query.Limit {
		candidates = candidates[:query.Limit]
	}
	return candidates, nil
}

func hasUnitNorm(values embedding.Vector) bool {
	var squaredNorm float64
	for _, value := range values {
		component := float64(value)
		squaredNorm += component * component
	}
	return math.Abs(math.Sqrt(squaredNorm)-1) <= unitNormTolerance
}

func cosineSimilarity(ctx context.Context, left, right embedding.Vector) (float64, error) {
	if len(left) != len(right) || len(left) == 0 {
		return 0, vectorsearch.ErrVectorIntegrity
	}
	var dot, leftSquaredNorm, rightSquaredNorm float64
	for position := range left {
		if position%256 == 0 {
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
	if encodingVersion != vectorEncodingVersion || dimensions <= 0 || len(encoded) == 0 || len(encoded)%4 != 0 || len(encoded)/4 != dimensions {
		return nil, vectorsearch.ErrVectorIntegrity
	}
	values := make(embedding.Vector, dimensions)
	for position := range values {
		values[position] = math.Float32frombits(binary.LittleEndian.Uint32(encoded[position*4:]))
	}
	if err := validateVectorComponents(values, dimensions); err != nil {
		return nil, vectorsearch.ErrVectorIntegrity
	}
	return values, nil
}

func validateVectorComponents(values embedding.Vector, dimensions int) error {
	if dimensions <= 0 || len(values) != dimensions {
		return errInvalidVectorEncoding
	}
	nonZero := false
	for _, value := range values {
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
