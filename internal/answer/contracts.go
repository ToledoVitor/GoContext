// Package answer defines evidence-grounded answer generation and validation.
package answer

import (
	"context"

	"github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/source"
)

// Draft is generator output before guardrail validation.
type Draft struct {
	Text      string
	Citations []source.Citation
}

// Generator produces an answer only from supplied, untrusted evidence.
type Generator interface {
	Generate(ctx context.Context, question string, evidence []search.Hit) (Draft, error)
}

// Guard validates citation integrity and repository policy before publication.
type Guard interface {
	Validate(ctx context.Context, draft Draft, evidence []search.Hit) error
}
