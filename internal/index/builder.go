package index

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"reflect"
	"strings"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	"github.com/ToledoVitor/GoContext/internal/source"
)

const (
	defaultMaxChunks      = 20_000
	defaultMaxSourceBytes = int64(64 * 1024 * 1024)
	builderContextStride  = 256
	generationIDVersion   = "generation-v1"
	lexicalOnlySpace      = "lexical-only"
	semanticSpace         = "semantic"

	// SemanticStatusOff reports that semantic indexing was intentionally disabled.
	SemanticStatusOff = "off"
	// SemanticStatusIndexed reports that a complete semantic generation was published.
	SemanticStatusIndexed = "indexed"
	// SemanticStatusDegraded reports that preferred semantic indexing fell back to lexical-only.
	SemanticStatusDegraded = "degraded"

	// DegradedReasonUnavailable is the stable, sanitized reason for preferred-mode fallback.
	DegradedReasonUnavailable = "semantic-unavailable"
)

var (
	// ErrInvalidBuilder reports invalid builder dependencies or configuration.
	ErrInvalidBuilder = errors.New("invalid index builder")
	// ErrInvalidCorpus reports an invalid repository or canonical corpus request.
	ErrInvalidCorpus = errors.New("invalid source corpus")
	// ErrCostLimit reports that a corpus exceeds configured local indexing limits.
	ErrCostLimit = errors.New("indexing cost limit exceeded")
	// ErrSemanticIntegrity reports malformed or incompatible embedding output.
	ErrSemanticIntegrity = errors.New("embedding result failed integrity validation")
	// ErrSemanticFailure reports a non-degradable embedding provider failure.
	ErrSemanticFailure = errors.New("semantic embedding failed")
	// ErrSemanticUnavailable is the provider-neutral exhausted temporary failure category.
	ErrSemanticUnavailable = embedding.ErrSemanticUnavailable
	// ErrStoreFailure reports a sanitized index store operation failure.
	ErrStoreFailure = errors.New("index store operation failed")
)

// BuilderConfig controls semantic policy and defensive corpus cost limits.
type BuilderConfig struct {
	Mode           SemanticMode
	MaxChunks      int
	MaxSourceBytes int64
}

// Report summarizes one attempted generation publication without source or provider details.
type Report struct {
	GenerationID   string
	CorpusRevision string
	Chunks         int
	Vectors        int
	Requests       int
	UsageTokens    int
	Semantic       string
	DegradedReason string
}

// Builder orchestrates complete provider-neutral repository generations.
type Builder struct {
	store          Store
	embedder       embedding.Embedder
	mode           SemanticMode
	maxChunks      int
	maxSourceBytes int64
}

// NewBuilder validates dependencies and applies safe indexing defaults.
func NewBuilder(store Store, embedder embedding.Embedder, config BuilderConfig) (*Builder, error) {
	if isNilDependency(store) || (embedder != nil && isNilDependency(embedder)) || config.MaxChunks < 0 || config.MaxSourceBytes < 0 {
		return nil, ErrInvalidBuilder
	}
	mode := config.Mode
	if mode == "" {
		mode = SemanticOff
	}
	if mode != SemanticOff && mode != SemanticPreferred && mode != SemanticRequired {
		return nil, ErrInvalidBuilder
	}
	if mode != SemanticOff && isNilDependency(embedder) {
		return nil, ErrInvalidBuilder
	}
	maxChunks := config.MaxChunks
	if maxChunks == 0 {
		maxChunks = defaultMaxChunks
	}
	maxSourceBytes := config.MaxSourceBytes
	if maxSourceBytes == 0 {
		maxSourceBytes = defaultMaxSourceBytes
	}
	return &Builder{
		store:          store,
		embedder:       embedder,
		mode:           mode,
		maxChunks:      maxChunks,
		maxSourceBytes: maxSourceBytes,
	}, nil
}

func isNilDependency(dependency any) bool {
	if dependency == nil {
		return true
	}
	value := reflect.ValueOf(dependency)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Replace builds and atomically publishes a complete repository generation.
func (b *Builder) Replace(ctx context.Context, repositoryID string, corpus source.Corpus) (Report, error) {
	if b == nil || isNilDependency(b.store) || ctx == nil ||
		(b.embedder != nil && isNilDependency(b.embedder)) || (b.mode != SemanticOff && b.embedder == nil) {
		return Report{}, ErrInvalidBuilder
	}
	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("build index generation: %w", err)
	}
	if strings.TrimSpace(repositoryID) == "" {
		return Report{}, ErrInvalidCorpus
	}
	if err := validateCost(ctx, corpus.Chunks, b.maxChunks, b.maxSourceBytes); err != nil {
		return Report{}, err
	}

	chunks, err := cloneChunksContext(ctx, corpus.Chunks)
	if err != nil {
		return Report{}, fmt.Errorf("build index generation: %w", err)
	}
	validated, err := source.NewCorpusContext(ctx, corpus.PolicyVersion, chunks)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return Report{}, fmt.Errorf("build index generation: %w", contextErr)
		}
		return Report{}, ErrInvalidCorpus
	}
	if validated.Revision != corpus.Revision {
		return Report{}, ErrInvalidCorpus
	}
	baseGeneration, err := b.activeGeneration(ctx, repositoryID)
	if err != nil {
		return Report{}, err
	}
	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("build index generation: %w", err)
	}

	if b.mode == SemanticOff {
		generation := b.lexicalGeneration(repositoryID, baseGeneration, corpus, chunks)
		report := Report{
			GenerationID:   generation.ID,
			CorpusRevision: corpus.Revision,
			Chunks:         len(chunks),
			Semantic:       SemanticStatusOff,
		}
		return b.publish(ctx, generation, report)
	}

	profile := b.embedder.Profile()
	if strings.TrimSpace(profile.Fingerprint) == "" || strings.TrimSpace(profile.Model) == "" {
		return Report{}, ErrSemanticIntegrity
	}
	texts := make([]string, len(chunks))
	for position, chunk := range chunks {
		if position%builderContextStride == 0 {
			if err := ctx.Err(); err != nil {
				return Report{}, fmt.Errorf("build index generation: %w", err)
			}
		}
		texts[position] = chunk.Text
	}
	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("build index generation: %w", err)
	}
	batch, embedErr := b.embedder.Embed(ctx, embedding.PurposeDocument, texts)
	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("build index generation: %w", err)
	}
	if embedErr != nil {
		if errors.Is(embedErr, embedding.ErrInvalidBatch) || errors.Is(embedErr, embedding.ErrInvalidVector) {
			return Report{}, ErrSemanticIntegrity
		}
		if errors.Is(embedErr, embedding.ErrSemanticUnavailable) {
			attemptedRequests := embedding.AttemptedRequests(embedErr)
			if b.mode == SemanticPreferred {
				generation := b.lexicalGeneration(repositoryID, baseGeneration, corpus, chunks)
				report := Report{
					GenerationID:   generation.ID,
					CorpusRevision: corpus.Revision,
					Chunks:         len(chunks),
					Requests:       attemptedRequests,
					Semantic:       SemanticStatusDegraded,
					DegradedReason: DegradedReasonUnavailable,
				}
				return b.publish(ctx, generation, report)
			}
			return Report{}, fmt.Errorf("build index generation: %w", embedding.NewSemanticUnavailable(attemptedRequests))
		}
		return Report{}, ErrSemanticFailure
	}
	if err := embedding.ValidateBatchContext(ctx, batch, len(chunks)); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Report{}, fmt.Errorf("build index generation: %w", err)
		}
		return Report{}, ErrSemanticIntegrity
	}
	if batch.Profile != profile || batch.UsageTokens < 0 {
		return Report{}, ErrSemanticIntegrity
	}
	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("build index generation: %w", err)
	}

	vectors := make([]VectorRecord, len(chunks))
	for position, chunk := range chunks {
		if position%builderContextStride == 0 {
			if err := ctx.Err(); err != nil {
				return Report{}, fmt.Errorf("build index generation: %w", err)
			}
		}
		values, err := copyVectorContext(ctx, batch.Vectors[position])
		if err != nil {
			return Report{}, fmt.Errorf("build index generation: %w", err)
		}
		vectors[position] = VectorRecord{
			ChunkID: chunk.ID,
			Values:  values,
		}
	}
	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("build index generation: %w", err)
	}
	persistedProfile := profile
	generation := Generation{
		RepositoryID:      repositoryID,
		ID:                generationID(corpus.PolicyVersion, corpus.Revision, semanticSpace, profile.Fingerprint),
		BaseGeneration:    baseGeneration,
		CorpusRevision:    corpus.Revision,
		ScanPolicyVersion: corpus.PolicyVersion,
		Chunks:            chunks,
		Profile:           &persistedProfile,
		Dimensions:        batch.Dimensions,
		Metric:            VectorMetricCosine,
		Vectors:           vectors,
	}
	report := Report{
		GenerationID:   generation.ID,
		CorpusRevision: corpus.Revision,
		Chunks:         len(chunks),
		Vectors:        len(vectors),
		Requests:       batch.Requests,
		UsageTokens:    batch.UsageTokens,
		Semantic:       SemanticStatusIndexed,
	}
	return b.publish(ctx, generation, report)
}

func (b *Builder) lexicalGeneration(repositoryID, baseGeneration string, corpus source.Corpus, chunks []source.Chunk) Generation {
	return Generation{
		RepositoryID:      repositoryID,
		ID:                generationID(corpus.PolicyVersion, corpus.Revision, lexicalOnlySpace, ""),
		BaseGeneration:    baseGeneration,
		CorpusRevision:    corpus.Revision,
		ScanPolicyVersion: corpus.PolicyVersion,
		Chunks:            chunks,
		Metric:            VectorMetricCosine,
	}
}

func (b *Builder) activeGeneration(ctx context.Context, repositoryID string) (string, error) {
	active, err := b.store.ActiveGeneration(ctx, repositoryID)
	if err == nil {
		return active, nil
	}
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	return "", sanitizeStoreError(ctx, err)
}

func (b *Builder) publish(ctx context.Context, generation Generation, report Report) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("build index generation: %w", err)
	}
	err := b.store.Replace(ctx, generation)
	if err == nil {
		return report, nil
	}
	var committed *CommittedCleanupError
	if errors.As(err, &committed) {
		return report, fmt.Errorf("build index generation: %w", committed)
	}
	return Report{}, sanitizeStoreError(ctx, err)
}

func sanitizeStoreError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return fmt.Errorf("build index generation: %w", contextErr)
		}
	}
	for _, category := range []error{
		context.Canceled,
		context.DeadlineExceeded,
		ErrConcurrentIndex,
		ErrInvalidGeneration,
		ErrReindexRequired,
	} {
		if errors.Is(err, category) {
			return fmt.Errorf("build index generation: %w", category)
		}
	}
	return ErrStoreFailure
}

func cloneChunksContext(ctx context.Context, chunks []source.Chunk) ([]source.Chunk, error) {
	clone := make([]source.Chunk, len(chunks))
	for offset := 0; offset < len(chunks); offset += builderContextStride {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := offset + builderContextStride
		if end > len(chunks) {
			end = len(chunks)
		}
		copy(clone[offset:end], chunks[offset:end])
	}
	return clone, ctx.Err()
}

func copyVectorContext(ctx context.Context, vector embedding.Vector) (embedding.Vector, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clone := make(embedding.Vector, len(vector))
	for offset := 0; offset < len(vector); offset += builderContextStride {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := offset + builderContextStride
		if end > len(vector) {
			end = len(vector)
		}
		copy(clone[offset:end], vector[offset:end])
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return clone, nil
}

func validateCost(ctx context.Context, chunks []source.Chunk, maxChunks int, maxSourceBytes int64) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("build index generation: %w", err)
	}
	if len(chunks) > maxChunks {
		return ErrCostLimit
	}
	var total int64
	for position, chunk := range chunks {
		if position%builderContextStride == 0 {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("build index generation: %w", err)
			}
		}
		chunkBytes := int64(len(chunk.Text))
		if chunkBytes > maxSourceBytes-total {
			return ErrCostLimit
		}
		total += chunkBytes
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("build index generation: %w", err)
	}
	return nil
}

func generationID(policyVersion, revision, space, fingerprint string) string {
	digest := sha256.New()
	writeGenerationIDPart(digest, generationIDVersion)
	writeGenerationIDPart(digest, policyVersion)
	writeGenerationIDPart(digest, revision)
	writeGenerationIDPart(digest, space)
	if space != lexicalOnlySpace {
		writeGenerationIDPart(digest, fingerprint)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func writeGenerationIDPart(writer hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write([]byte(value))
}
