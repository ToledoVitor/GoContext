// Package embedding defines provider-agnostic embedding values and operations.
package embedding

import (
	"context"
	"errors"
	"math"
)

var (
	// ErrInvalidBatch reports invalid embedding batch metadata or shape.
	ErrInvalidBatch = errors.New("invalid embedding batch")
	// ErrInvalidVector reports an invalid vector dimension or value.
	ErrInvalidVector = errors.New("invalid embedding vector")
)

// Purpose identifies how an embedding will be used.
type Purpose string

const (
	PurposeDocument Purpose = "document"
	PurposeQuery    Purpose = "query"
)

// Profile identifies the configuration that produced an embedding.
type Profile struct {
	Fingerprint string
	Model       string
}

// Vector is an embedding represented with float32 components.
type Vector []float32

// Batch contains ordered embeddings and provider usage metadata.
type Batch struct {
	Profile     Profile
	Dimensions  int
	Vectors     []Vector
	Requests    int
	UsageTokens int
}

// Embedder produces embeddings while preserving input quantity and order.
type Embedder interface {
	Profile() Profile
	Embed(ctx context.Context, purpose Purpose, texts []string) (Batch, error)
}

// ValidateBatch verifies embedding metadata, shape, and values.
func ValidateBatch(batch Batch, expected int) error {
	if batch.Profile.Fingerprint == "" || batch.Profile.Model == "" || batch.Dimensions <= 0 || batch.Requests <= 0 || len(batch.Vectors) != expected {
		return ErrInvalidBatch
	}

	for _, vector := range batch.Vectors {
		if len(vector) != batch.Dimensions {
			return ErrInvalidVector
		}

		nonZero := false
		for _, value := range vector {
			component := float64(value)
			if math.IsNaN(component) || math.IsInf(component, 0) {
				return ErrInvalidVector
			}
			nonZero = nonZero || value != 0
		}
		if !nonZero {
			return ErrInvalidVector
		}
	}

	return nil
}
