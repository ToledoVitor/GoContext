// Package lexical implements deterministic token-based retrieval over chunk snapshots.
package lexical

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/source"
)

const (
	textWeight   = 0.6
	symbolWeight = 0.3
	pathWeight   = 0.1
)

var (
	ErrInvalidQuery  = errors.New("invalid lexical query")
	ErrInvalidLoader = errors.New("invalid snapshot loader")
)

// SnapshotLoader provides repository chunks without coupling search to storage.
type SnapshotLoader interface {
	Load(ctx context.Context, repositoryID string) ([]source.Chunk, error)
}

// Searcher ranks exact lexical token matches from one repository snapshot.
type Searcher struct {
	loader SnapshotLoader
}

// NewSearcher creates a lexical searcher backed by repository snapshots.
func NewSearcher(loader SnapshotLoader) (*Searcher, error) {
	if loader == nil {
		return nil, ErrInvalidLoader
	}
	return &Searcher{loader: loader}, nil
}

// Search loads one snapshot, scores matching chunks, and returns stable ranking.
func (s *Searcher) Search(ctx context.Context, query search.Query) ([]search.Hit, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("search repository snapshot: %w", err)
	}
	queryTerms := uniqueTokens(query.Text)
	if strings.TrimSpace(query.RepositoryID) == "" || len(queryTerms) == 0 || query.Limit <= 0 {
		return nil, ErrInvalidQuery
	}

	chunks, err := s.loader.Load(ctx, query.RepositoryID)
	if err != nil {
		return nil, fmt.Errorf("search repository snapshot: %w", err)
	}

	hits := make([]search.Hit, 0, len(chunks))
	for _, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("search repository snapshot: %w", err)
		}
		score := scoreChunk(queryTerms, chunk)
		if score > 0 {
			hits = append(hits, search.Hit{Chunk: chunk, Score: score})
		}
	}

	sort.Slice(hits, func(left, right int) bool {
		if hits[left].Score != hits[right].Score {
			return hits[left].Score > hits[right].Score
		}
		leftReference := hits[left].Chunk.Reference
		rightReference := hits[right].Chunk.Reference
		if leftReference.Path != rightReference.Path {
			return leftReference.Path < rightReference.Path
		}
		if leftReference.StartLine != rightReference.StartLine {
			return leftReference.StartLine < rightReference.StartLine
		}
		return hits[left].Chunk.ID < hits[right].Chunk.ID
	})

	if len(hits) > query.Limit {
		hits = hits[:query.Limit]
	}
	return hits, nil
}

func scoreChunk(queryTerms []string, chunk source.Chunk) float64 {
	textTerms := tokenSet(chunk.Text)
	symbolTerms := tokenSet(chunk.SymbolName)
	pathTerms := tokenSet(chunk.Reference.Path)

	var score float64
	for _, term := range queryTerms {
		if _, matched := textTerms[term]; matched {
			score += textWeight
		}
		if _, matched := symbolTerms[term]; matched {
			score += symbolWeight
		}
		if _, matched := pathTerms[term]; matched {
			score += pathWeight
		}
	}
	return score / float64(len(queryTerms))
}

func tokenSet(value string) map[string]struct{} {
	tokens := tokenize(value)
	set := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		set[token] = struct{}{}
	}
	return set
}

func uniqueTokens(value string) []string {
	tokens := tokenize(value)
	unique := make([]string, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		unique = append(unique, token)
	}
	return unique
}

func tokenize(value string) []string {
	runes := []rune(value)
	tokens := make([]string, 0)
	current := make([]rune, 0)
	flush := func() {
		if len(current) == 0 {
			return
		}
		tokens = append(tokens, string(current))
		current = current[:0]
	}

	for index, character := range runes {
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) {
			flush()
			continue
		}

		if unicode.IsUpper(character) && len(current) > 0 {
			previous := runes[index-1]
			nextIsLower := index+1 < len(runes) && unicode.IsLower(runes[index+1])
			if unicode.IsLower(previous) || unicode.IsDigit(previous) || (unicode.IsUpper(previous) && nextIsLower) {
				flush()
			}
		}
		current = append(current, unicode.ToLower(character))
	}
	flush()
	return tokens
}

var _ search.Searcher = (*Searcher)(nil)
