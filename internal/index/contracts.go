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
	ErrCleanupIncomplete = errors.New("published index cleanup incomplete")
)

// CleanupStage identifies post-commit maintenance that did not complete.
type CleanupStage string

const (
	CleanupStagePurge      CleanupStage = "purge"
	CleanupStageCheckpoint CleanupStage = "checkpoint"
)

// CommittedCleanupError reports that publication committed but post-commit
// maintenance needs a safe retry. Its message intentionally excludes the
// underlying database error so source-bearing database state cannot leak.
type CommittedCleanupError struct {
	stage CleanupStage
	cause error
}

// NewCommittedCleanupError creates an explicit committed publication outcome.
func NewCommittedCleanupError(stage CleanupStage, cause error) *CommittedCleanupError {
	return &CommittedCleanupError{stage: stage, cause: cause}
}

func (e *CommittedCleanupError) Error() string {
	return "index generation published; cleanup incomplete"
}

// Unwrap retains categorical and operational matching without exposing either
// through the user-facing error string.
func (e *CommittedCleanupError) Unwrap() []error {
	if e == nil || e.cause == nil {
		return []error{ErrCleanupIncomplete}
	}
	return []error{ErrCleanupIncomplete, e.cause}
}

// Published reports that the manifest update committed successfully.
func (e *CommittedCleanupError) Published() bool {
	return e != nil
}

// Stage reports which post-commit maintenance step needs retrying.
func (e *CommittedCleanupError) Stage() CleanupStage {
	if e == nil {
		return ""
	}
	return e.stage
}

// SemanticMode controls whether semantic indexing is disabled, preferred, or required.
type SemanticMode string

const (
	SemanticOff       SemanticMode = "off"
	SemanticPreferred SemanticMode = "preferred"
	SemanticRequired  SemanticMode = "required"
)

// VectorMetric identifies the provider-neutral similarity semantics of stored vectors.
type VectorMetric string

const (
	// VectorMetricCosine is the exact cosine metric used by the initial vector reader.
	VectorMetricCosine VectorMetric = "cosine"
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
	Metric            VectorMetric
	Vectors           []VectorRecord
}

// Store publishes complete repository generations.
type Store interface {
	ActiveGeneration(context.Context, string) (string, error)
	Replace(context.Context, Generation) error
}
