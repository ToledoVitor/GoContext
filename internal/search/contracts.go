// Package search defines retrieval inputs and outputs independently of index engines.
package search

import (
	"context"

	"github.com/ToledoVitor/GoContext/internal/source"
)

// Query is a repository-scoped retrieval request.
type Query struct {
	RepositoryID string
	Text         string
	Limit        int
}

// Hit is a ranked source chunk with a normalized score.
type Hit struct {
	Chunk source.Chunk
	Score float64
}

// Searcher retrieves evidence. Hybrid ranking remains an implementation detail.
type Searcher interface {
	Search(ctx context.Context, query Query) ([]Hit, error)
}
