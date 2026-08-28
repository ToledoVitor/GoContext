// Package search defines retrieval inputs and outputs independently of index engines.
package search

import (
	"context"
	"errors"
	"strings"

	"github.com/ToledoVitor/GoContext/internal/source"
)

// ErrInvalidFilter reports a filter that cannot be applied to canonical source chunks.
var ErrInvalidFilter = errors.New("invalid search filter")

// Filter restricts retrieval to repository-relative path prefixes and source languages.
// Values within one category are ORed; non-empty categories are ANDed together.
type Filter struct {
	PathPrefixes []string
	Languages    []source.Language
}

// Query is a repository-scoped retrieval request.
type Query struct {
	RepositoryID string
	Text         string
	Limit        int
	Filter       Filter
}

// Hit is a ranked source chunk with a normalized score.
type Hit struct {
	Chunk source.Chunk
	Score float64
}

// Searcher retrieves evidence. Hybrid ranking remains an implementation detail.
type Searcher interface {
	Search(ctx context.Context, query Query) ([]Hit, error)
}

// ValidateFilter rejects path prefixes that cannot address canonical source references.
func ValidateFilter(filter Filter) error {
	for _, prefix := range filter.PathPrefixes {
		if _, valid := normalizedPathPrefix(prefix); !valid {
			return ErrInvalidFilter
		}
	}
	for _, language := range filter.Languages {
		if !supportedLanguage(language) {
			return ErrInvalidFilter
		}
	}
	return nil
}

// MatchesFilter reports whether a chunk belongs to every populated filter category.
// Callers validate the filter once with ValidateFilter before matching chunks.
func MatchesFilter(chunk source.Chunk, filter Filter) bool {
	if len(filter.PathPrefixes) > 0 {
		pathMatched := false
		for _, prefix := range filter.PathPrefixes {
			normalized, valid := normalizedPathPrefix(prefix)
			if valid && (chunk.Reference.Path == normalized || strings.HasPrefix(chunk.Reference.Path, normalized+"/")) {
				pathMatched = true
				break
			}
		}
		if !pathMatched {
			return false
		}
	}

	if len(filter.Languages) > 0 {
		for _, language := range filter.Languages {
			if chunk.Language == language {
				return true
			}
		}
		return false
	}
	return true
}

func normalizedPathPrefix(prefix string) (string, bool) {
	normalized := strings.TrimSuffix(prefix, "/")
	reference := source.Reference{Path: normalized, StartLine: 1, EndLine: 1}
	return normalized, reference.Valid()
}

func supportedLanguage(language source.Language) bool {
	switch language {
	case source.LanguagePython, source.LanguageTypeScript:
		return true
	default:
		return false
	}
}
