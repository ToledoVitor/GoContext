// Package ingest defines the repository-to-chunks pipeline.
package ingest

import (
	"context"

	"github.com/ToledoVitor/GoContext/internal/source"
)

// ScanPolicyVersion identifies all scanner decisions that affect safe corpus
// eligibility. A change to those decisions requires a new version.
const ScanPolicyVersion = "scanner-v4"

// ExclusionReason is a sanitized, aggregate-only scanner decision.
type ExclusionReason string

const (
	ExclusionSecurity             ExclusionReason = "security"
	ExclusionDependencyBuildCache ExclusionReason = "dependency_build_cache"
	ExclusionNestedRepository     ExclusionReason = "nested_repository"
	ExclusionSymlink              ExclusionReason = "symlink"
	ExclusionUnsupportedExtension ExclusionReason = "unsupported_extension"
	ExclusionNonRegular           ExclusionReason = "non_regular"
	ExclusionTooLarge             ExclusionReason = "too_large"
	ExclusionBinary               ExclusionReason = "binary"
	ExclusionInvalidUTF8          ExclusionReason = "invalid_utf8"
	ExclusionGenerated            ExclusionReason = "generated"
	ExclusionSecret               ExclusionReason = "secret"
)

// ScanReport contains only aggregate inventory and exclusion statistics.
type ScanReport struct {
	EligibleFiles               int
	EligibleBytes               int64
	IncludedFiles               int
	IncludedBytes               int64
	Excluded                    map[ExclusionReason]int
	IncludedByLanguage          map[source.Language]int
	SizeBands                   map[string]int
	UnsupportedByExtension      map[string]int
	UnsupportedBytesByExtension map[string]int64
}

// ScanResult binds eligible source files to the policy and audit report that
// produced them.
type ScanResult struct {
	PolicyVersion string
	Files         []source.File
	Report        ScanReport
}

// Scanner discovers eligible files below an authorized repository root.
type Scanner interface {
	Scan(ctx context.Context, root string) (ScanResult, error)
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
	Replace(ctx context.Context, repositoryID string, corpus source.Corpus) error
}
