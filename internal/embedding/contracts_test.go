package embedding_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/embedding"
)

func TestSemanticUnavailableCarriesOnlySanitizedAttemptMetadata(t *testing.T) {
	for _, requests := range []int{-1, 0, 3} {
		err := embedding.NewSemanticUnavailable(requests)
		if !errors.Is(err, embedding.ErrSemanticUnavailable) {
			t.Fatalf("NewSemanticUnavailable(%d) error = %v, want ErrSemanticUnavailable", requests, err)
		}
		want := requests
		if want < 0 {
			want = 0
		}
		if got := embedding.AttemptedRequests(fmt.Errorf("safe wrapper: %w", err)); got != want {
			t.Fatalf("AttemptedRequests(NewSemanticUnavailable(%d)) = %d, want %d", requests, got, want)
		}
		if got := err.Error(); got != embedding.ErrSemanticUnavailable.Error() {
			t.Fatalf("NewSemanticUnavailable(%d).Error() = %q, want sanitized sentinel text", requests, got)
		}
	}
	if got := embedding.AttemptedRequests(errors.New("provider raw cause")); got != 0 {
		t.Fatalf("AttemptedRequests(untyped error) = %d, want 0", got)
	}
}

func TestValidateBatchAcceptsOrderedFiniteVectors(t *testing.T) {
	batch := embedding.Batch{
		Profile:    embedding.Profile{Fingerprint: "profile", Model: "model"},
		Dimensions: 2,
		Vectors:    []embedding.Vector{{1, 0}, {0, 1}},
		Requests:   1,
	}
	if err := embedding.ValidateBatch(batch, 2); err != nil {
		t.Fatalf("ValidateBatch() error = %v", err)
	}
}

func TestValidateBatchDoesNotMutateBatch(t *testing.T) {
	batch := embedding.Batch{
		Profile:    embedding.Profile{Fingerprint: "profile", Model: "model"},
		Dimensions: 2,
		Vectors:    []embedding.Vector{{1, 0}, {0, 1}},
		Requests:   1,
	}
	before := cloneBatch(batch)

	if err := embedding.ValidateBatch(batch, 2); err != nil {
		t.Fatalf("ValidateBatch() error = %v", err)
	}
	if !reflect.DeepEqual(batch, before) {
		t.Fatalf("ValidateBatch() mutated batch = %#v, want %#v", batch, before)
	}
}

func TestValidateBatchRejectsInvalidShapeAndValues(t *testing.T) {
	tests := []struct {
		name     string
		batch    embedding.Batch
		expected int
		want     error
	}{
		{
			name:     "empty profile",
			batch:    embedding.Batch{Profile: embedding.Profile{}, Dimensions: 2, Vectors: []embedding.Vector{{1, 0}}, Requests: 1},
			expected: 1,
			want:     embedding.ErrInvalidBatch,
		},
		{
			name:     "missing fingerprint",
			batch:    embedding.Batch{Profile: embedding.Profile{Model: "m"}, Dimensions: 2, Vectors: []embedding.Vector{{1, 0}}, Requests: 1},
			expected: 1,
			want:     embedding.ErrInvalidBatch,
		},
		{
			name:     "missing model",
			batch:    embedding.Batch{Profile: embedding.Profile{Fingerprint: "p"}, Dimensions: 2, Vectors: []embedding.Vector{{1, 0}}, Requests: 1},
			expected: 1,
			want:     embedding.ErrInvalidBatch,
		},
		{
			name:     "zero dimensions",
			batch:    embedding.Batch{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Vectors: []embedding.Vector{{1}}, Requests: 1},
			expected: 1,
			want:     embedding.ErrInvalidBatch,
		},
		{
			name:     "negative dimensions",
			batch:    embedding.Batch{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: -1, Vectors: []embedding.Vector{{1}}, Requests: 1},
			expected: 1,
			want:     embedding.ErrInvalidBatch,
		},
		{
			name:     "missing vectors",
			batch:    embedding.Batch{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 2, Requests: 1},
			expected: 1,
			want:     embedding.ErrInvalidBatch,
		},
		{
			name:     "extra vectors",
			batch:    embedding.Batch{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 2, Vectors: []embedding.Vector{{1, 0}, {0, 1}}, Requests: 1},
			expected: 1,
			want:     embedding.ErrInvalidBatch,
		},
		{
			name:     "inconsistent dimensions",
			batch:    embedding.Batch{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 2, Vectors: []embedding.Vector{{1}}, Requests: 1},
			expected: 1,
			want:     embedding.ErrInvalidVector,
		},
		{
			name:     "NaN component",
			batch:    embedding.Batch{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 1, Vectors: []embedding.Vector{{float32(math.NaN())}}, Requests: 1},
			expected: 1,
			want:     embedding.ErrInvalidVector,
		},
		{
			name:     "positive infinity component",
			batch:    embedding.Batch{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 1, Vectors: []embedding.Vector{{float32(math.Inf(1))}}, Requests: 1},
			expected: 1,
			want:     embedding.ErrInvalidVector,
		},
		{
			name:     "negative infinity component",
			batch:    embedding.Batch{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 1, Vectors: []embedding.Vector{{float32(math.Inf(-1))}}, Requests: 1},
			expected: 1,
			want:     embedding.ErrInvalidVector,
		},
		{
			name:     "zero vector",
			batch:    embedding.Batch{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 2, Vectors: []embedding.Vector{{0, 0}}, Requests: 1},
			expected: 1,
			want:     embedding.ErrInvalidVector,
		},
		{
			name:     "zero requests",
			batch:    embedding.Batch{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 2, Vectors: []embedding.Vector{{1, 0}}},
			expected: 1,
			want:     embedding.ErrInvalidBatch,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := embedding.ValidateBatch(test.batch, test.expected); !errors.Is(err, test.want) {
				t.Fatalf("ValidateBatch(%#v, %d) error = %v, want %v", test.batch, test.expected, err, test.want)
			}
		})
	}
}

func TestValidateBatchContextMatchesLegacyContract(t *testing.T) {
	validProfile := embedding.Profile{Fingerprint: "profile", Model: "model"}
	tests := []struct {
		name     string
		batch    embedding.Batch
		expected int
		want     error
	}{
		{
			name: "valid",
			batch: embedding.Batch{
				Profile:     validProfile,
				Dimensions:  2,
				Vectors:     []embedding.Vector{{1, 0}},
				Requests:    1,
				UsageTokens: 1,
			},
			expected: 1,
		},
		{
			name: "negative usage remains outside the embedding shape contract",
			batch: embedding.Batch{
				Profile:     validProfile,
				Dimensions:  2,
				Vectors:     []embedding.Vector{{1, 0}},
				Requests:    1,
				UsageTokens: -1,
			},
			expected: 1,
		},
		{
			name: "invalid metadata",
			batch: embedding.Batch{
				Profile:    validProfile,
				Dimensions: 2,
				Vectors:    []embedding.Vector{{1, 0}},
			},
			expected: 1,
			want:     embedding.ErrInvalidBatch,
		},
		{
			name: "invalid vector",
			batch: embedding.Batch{
				Profile:    validProfile,
				Dimensions: 2,
				Vectors:    []embedding.Vector{{0, 0}},
				Requests:   1,
			},
			expected: 1,
			want:     embedding.ErrInvalidVector,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			legacyErr := embedding.ValidateBatch(test.batch, test.expected)
			contextErr := embedding.ValidateBatchContext(context.Background(), test.batch, test.expected)

			if !sameValidationOutcome(legacyErr, test.want) {
				t.Fatalf("ValidateBatch() error = %v, want %v", legacyErr, test.want)
			}
			if !sameValidationOutcome(contextErr, test.want) {
				t.Fatalf("ValidateBatchContext() error = %v, want %v", contextErr, test.want)
			}
		})
	}
}

func TestValidateBatchContextCancelsDuringSingleHugeVector(t *testing.T) {
	const dimensions = 1 << 20
	vector := make(embedding.Vector, dimensions)
	vector[0] = 1
	ctx := newValidationCancelContext(32)
	batch := embedding.Batch{
		Profile:    embedding.Profile{Fingerprint: "profile", Model: "model"},
		Dimensions: dimensions,
		Vectors:    []embedding.Vector{vector},
		Requests:   1,
	}

	err := embedding.ValidateBatchContext(ctx, batch, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ValidateBatchContext() error = %v, want context.Canceled", err)
	}
	if calls := ctx.errCalls(); calls < 32 {
		t.Fatalf("context Err() calls = %d, want cancellation threshold reached", calls)
	}
}

func sameValidationOutcome(got, want error) bool {
	if want == nil {
		return got == nil
	}
	return errors.Is(got, want)
}

type validationCancelContext struct {
	context.Context
	mu       sync.Mutex
	cancelAt int
	calls    int
	done     chan struct{}
	once     sync.Once
}

func newValidationCancelContext(cancelAt int) *validationCancelContext {
	return &validationCancelContext{
		Context:  context.Background(),
		cancelAt: cancelAt,
		done:     make(chan struct{}),
	}
}

func (c *validationCancelContext) Done() <-chan struct{} {
	return c.done
}

func (c *validationCancelContext) Err() error {
	c.mu.Lock()
	c.calls++
	canceled := c.calls >= c.cancelAt
	c.mu.Unlock()
	if !canceled {
		return nil
	}
	c.once.Do(func() { close(c.done) })
	return context.Canceled
}

func (c *validationCancelContext) errCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func cloneBatch(batch embedding.Batch) embedding.Batch {
	clone := batch
	clone.Vectors = make([]embedding.Vector, len(batch.Vectors))
	for index, vector := range batch.Vectors {
		clone.Vectors[index] = append(embedding.Vector(nil), vector...)
	}
	return clone
}
