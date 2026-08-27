// Package ingest defines the repository-to-chunks pipeline.
package ingest

import (
	"context"

	"github.com/ToledoVitor/GoContext/internal/source"
)

// Scanner discovers eligible files below an authorized repository root.
type Scanner interface {
	Scan(ctx context.Context, root string) ([]source.File, error)
}

// Parser extracts declarations without discarding source locations.
type Parser interface {
	Parse(ctx context.Context, file source.File) ([]source.Symbol, error)
}

// Chunker turns a parsed file into independently retrievable source units.
type Chunker interface {
	Chunk(ctx context.Context, file source.File, symbols []source.Symbol) ([]source.Chunk, error)
}

// Store atomically replaces chunks belonging to one repository snapshot.
type Store interface {
	Replace(ctx context.Context, repositoryID string, chunks []source.Chunk) error
}
