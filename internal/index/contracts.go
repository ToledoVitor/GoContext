// Package index defines provider-agnostic index generations and publication.
package index

import (
	"context"
	"errors"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	"github.com/ToledoVitor/GoContext/internal/source"
)

var (
	ErrNotFound          = errors.New("repository index not found")
	ErrInvalidGeneration = errors.New("invalid index generation")
	ErrConcurrentIndex   = errors.New("concurrent index publication")
	ErrReindexRequired   = errors.New("repository index requires reindex")
)

// SemanticMode controls whether semantic indexing is disabled, preferred, or required.
type SemanticMode string

const (
	SemanticOff       SemanticMode = "off"
	SemanticPreferred SemanticMode = "preferred"
	SemanticRequired  SemanticMode = "required"
)

// VectorRecord associates one canonical chunk with its embedding.
type VectorRecord struct {
	ChunkID string
	Values  embedding.Vector
}

// Generation is a complete repository corpus ready for atomic publication.
type Generation struct {
	RepositoryID      string
	ID                string
	BaseGeneration    string
	CorpusRevision    string
	ScanPolicyVersion string
	Chunks            []source.Chunk
	Profile           *embedding.Profile
	Dimensions        int
	Vectors           []VectorRecord
}

// Store publishes complete repository generations.
type Store interface {
	ActiveGeneration(context.Context, string) (string, error)
	Replace(context.Context, Generation) error
}
