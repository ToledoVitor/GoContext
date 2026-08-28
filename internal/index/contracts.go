// Package index defines provider-agnostic index generations and publication.
package index

import (
	"context"
	"errors"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	"github.com/ToledoVitor/GoContext/internal/source"
)

var (
	ErrNotFound                = errors.New("repository index not found")
	ErrInvalidGeneration       = errors.New("invalid index generation")
	ErrConcurrentIndex         = errors.New("concurrent index publication")
	ErrReindexRequired         = errors.New("repository index requires reindex")
	ErrCleanupIncomplete       = errors.New("published index cleanup incomplete")
	ErrCommittedInfrastructure = errors.New("published index connection finalization incomplete")
)

// CleanupStage identifies post-commit maintenance that did not complete.
type CleanupStage string

const (
	CleanupStagePurge                   CleanupStage = "purge"
	CleanupStageCheckpoint              CleanupStage = "checkpoint"
	CleanupStagePublicationFinalization CleanupStage = "publication-finalization"
	CleanupStageConnectionRestore       CleanupStage = "connection-restore"
	CleanupStageConnectionRelease       CleanupStage = "connection-release"
)

// CommittedCleanupError reports that publication committed but post-commit
// cleanup or connection finalization needs a safe retry. Its message
// intentionally excludes the underlying database error so source-bearing
// database state cannot leak.
type CommittedCleanupError struct {
	stage CleanupStage
}

// NewCommittedCleanupError creates an explicit committed publication outcome.
func NewCommittedCleanupError(stage CleanupStage) *CommittedCleanupError {
	return &CommittedCleanupError{stage: stage}
}

func (e *CommittedCleanupError) Error() string {
	return "index generation published; post-commit maintenance incomplete"
}

// Unwrap exposes only a public outcome category. Raw database causes remain
// private so formatting, logging, errors.As, and recursive traversal cannot
// recover source-bearing database state.
func (e *CommittedCleanupError) Unwrap() error {
	if e != nil && (e.stage == CleanupStagePurge || e.stage == CleanupStageCheckpoint) {
		return ErrCleanupIncomplete
	}
	return ErrCommittedInfrastructure
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
