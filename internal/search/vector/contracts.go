// Package vector defines provider-neutral exact vector retrieval contracts.
package vector

import (
	"context"
	"errors"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	"github.com/ToledoVitor/GoContext/internal/index"
	"github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/source"
)

var (
	// ErrInvalidQueryVector reports a query vector that cannot be scored safely.
	ErrInvalidQueryVector = errors.New("invalid query vector")
	// ErrIncompatibleSpace reports a query and index with different embedding semantics.
	ErrIncompatibleSpace = errors.New("incompatible vector space")
	// ErrVectorUnavailable reports an active lexical-only generation.
	ErrVectorUnavailable = errors.New("vector index unavailable")
	// ErrVectorIntegrity reports malformed or incomplete persisted vector state.
	ErrVectorIntegrity = errors.New("vector index integrity failure")
	// ErrGenerationChanged reports a query that no longer matches pinned metadata.
	ErrGenerationChanged = errors.New("vector index generation changed")
)

// Metadata describes the vector space pinned by a bound index reader.
type Metadata struct {
	GenerationID   string
	CorpusRevision string
	Profile        embedding.Profile
	Dimensions     int
	Metric         index.VectorMetric
}

// IndexQuery requests an exact candidate scan in an expected pinned vector space.
type IndexQuery struct {
	RepositoryID string
	GenerationID string
	Profile      embedding.Profile
	Dimensions   int
	Metric       index.VectorMetric
	Vector       embedding.Vector
	Filter       search.Filter
	Limit        int
}

// Candidate is a canonical source chunk with its raw cosine similarity.
type Candidate struct {
	Chunk      source.Chunk
	Similarity float64
}

// Index exposes metadata and exact search over one bound generation.
type Index interface {
	Describe(ctx context.Context, repositoryID string) (Metadata, error)
	Search(ctx context.Context, query IndexQuery) ([]Candidate, error)
}
