package embedding_test

import (
	"math"
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

func TestValidateBatchRejectsInvalidShapeAndValues(t *testing.T) {
	tests := []embedding.Batch{
		{Profile: embedding.Profile{}, Dimensions: 2, Vectors: []embedding.Vector{{1, 0}}, Requests: 1},
		{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 2, Requests: 1},
		{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 2, Vectors: []embedding.Vector{{1}}, Requests: 1},
		{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 1, Vectors: []embedding.Vector{{float32(math.NaN())}}, Requests: 1},
		{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 2, Vectors: []embedding.Vector{{0, 0}}, Requests: 1},
		{Profile: embedding.Profile{Fingerprint: "p", Model: "m"}, Dimensions: 2, Vectors: []embedding.Vector{{1, 0}}},
	}
	for _, batch := range tests {
		if err := embedding.ValidateBatch(batch, 1); err == nil {
			t.Errorf("ValidateBatch(%#v) error = nil", batch)
		}
	}
}
