package embedding_test

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/embedding"
)

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

func cloneBatch(batch embedding.Batch) embedding.Batch {
	clone := batch
	clone.Vectors = make([]embedding.Vector, len(batch.Vectors))
	for index, vector := range batch.Vectors {
		clone.Vectors[index] = append(embedding.Vector(nil), vector...)
	}
	return clone
}
