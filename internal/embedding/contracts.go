// Package embedding defines provider-agnostic embedding values and operations.
package embedding

import (
	"context"
	"errors"
	"math"
)

var (
	// ErrSemanticUnavailable reports an exhausted temporary embedding failure.
	ErrSemanticUnavailable = errors.New("semantic embedding unavailable")
	// ErrInvalidBatch reports invalid embedding batch metadata or shape.
	ErrInvalidBatch = errors.New("invalid embedding batch")
	// ErrInvalidVector reports an invalid vector dimension or value.
	ErrInvalidVector = errors.New("invalid embedding vector")
)

type semanticUnavailableError struct {
	attemptedRequests int
}

func (err *semanticUnavailableError) Error() string {
	return ErrSemanticUnavailable.Error()
}

func (err *semanticUnavailableError) Unwrap() error {
	return ErrSemanticUnavailable
}

func (err *semanticUnavailableError) AttemptedRequests() int {
	if err == nil || err.attemptedRequests < 0 {
		return 0
	}
	return err.attemptedRequests
}

// NewSemanticUnavailable returns the provider-neutral exhausted temporary
// failure category with only a sanitized count of attempted requests.
func NewSemanticUnavailable(attemptedRequests int) error {
	if attemptedRequests < 0 {
		attemptedRequests = 0
	}
	return &semanticUnavailableError{attemptedRequests: attemptedRequests}
}

// AttemptedRequests returns sanitized attempt metadata when an error carries
// it, and zero for all other errors.
func AttemptedRequests(err error) int {
	var counted interface{ AttemptedRequests() int }
	if !errors.As(err, &counted) {
		return 0
	}
	requests := counted.AttemptedRequests()
	if requests < 0 {
		return 0
	}
	return requests
}

const validateBatchContextStride = 256

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
	return ValidateBatchContext(context.Background(), batch, expected)
}

// ValidateBatchContext verifies embedding metadata, shape, and values while
// allowing cancellation to interrupt validation of large batches or vectors.
func ValidateBatchContext(ctx context.Context, batch Batch, expected int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if batch.Profile.Fingerprint == "" || batch.Profile.Model == "" || batch.Dimensions <= 0 || batch.Requests <= 0 || len(batch.Vectors) != expected {
		return ErrInvalidBatch
	}

	for vectorPosition, vector := range batch.Vectors {
		if vectorPosition%validateBatchContextStride == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if len(vector) != batch.Dimensions {
			return ErrInvalidVector
		}

		nonZero := false
		for componentPosition, value := range vector {
			if componentPosition%validateBatchContextStride == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
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

	return ctx.Err()
}
