package lexical_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
	"github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/search/lexical"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestSearcherRanksByTermCoverageAndSourceFields(t *testing.T) {
	store := newStore(t)
	chunks := []source.Chunk{
		chunk("best", "services/user.py", 1, "LoadUser", "def LoadUser():\n    return repository.find_user()"),
		chunk("partial", "services/load.py", 1, "Load", "def load():\n    pass"),
		chunk("unmatched", "services/health.py", 1, "Health", "def health():\n    return True"),
	}
	if err := store.Replace(context.Background(), "repo", mustLexicalCorpus(t, chunks)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	searcher, err := lexical.NewSearcher(store)
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}

	hits, err := searcher.Search(context.Background(), search.Query{
		RepositoryID: "repo",
		Text:         "load user",
		Limit:        10,
		Filter:       search.Filter{},
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}

	if len(hits) != 2 {
		t.Fatalf("Search() returned %d hits, want 2: %#v", len(hits), hits)
	}
	if hits[0].Chunk.ID != "best" || hits[1].Chunk.ID != "partial" {
		t.Fatalf("Search() IDs = [%s %s], want [best partial]", hits[0].Chunk.ID, hits[1].Chunk.ID)
	}
	assertScore(t, hits[0].Score, 0.95)
	assertScore(t, hits[1].Score, 0.50)
	for index, hit := range hits {
		if hit.Score <= 0 || hit.Score > 1 {
			t.Errorf("hits[%d].Score = %f, want normalized score in (0, 1]", index, hit.Score)
		}
	}
}

func TestSearcherFiltersByPathPrefixBeforeLimit(t *testing.T) {
	store := newStore(t)
	chunks := []source.Chunk{
		chunk("internal", "internal/search/searcher.go", 1, "Search", "search token"),
		chunk("internalized", "internalized/searcher.go", 1, "Search", "search token"),
		chunk("package", "pkg/search/searcher.go", 1, "Search", "search token"),
		chunk("command", "cmd/search.go", 1, "Search", "search token"),
	}
	if err := store.Replace(context.Background(), "repo", mustLexicalCorpus(t, chunks)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	searcher, err := lexical.NewSearcher(store)
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}

	hits, err := searcher.Search(context.Background(), search.Query{
		RepositoryID: "repo",
		Text:         "search",
		Limit:        2,
		Filter: search.Filter{
			PathPrefixes: []string{"internal/", "pkg/"},
		},
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := hitIDs(hits); len(got) != 2 || got[0] != "internal" || got[1] != "package" {
		t.Fatalf("Search() IDs = %v, want [internal package]", got)
	}
}

func TestSearcherFiltersLanguagesWithinPathPrefixes(t *testing.T) {
	store := newStore(t)
	pythonInternal := chunk("python-internal", "internal/python/search.py", 1, "Search", "search token")
	typeScriptInternal := chunk("typescript-internal", "internal/typescript/search.ts", 1, "Search", "search token")
	typeScriptInternal.Language = source.LanguageTypeScript
	unknownInternal := chunk("unknown-internal", "internal/unknown/search.txt", 1, "Search", "search token")
	unknownInternal.Language = source.LanguageUnknown
	pythonPackage := chunk("python-package", "pkg/search.py", 1, "Search", "search token")
	chunks := []source.Chunk{pythonInternal, typeScriptInternal, unknownInternal, pythonPackage}
	if err := store.Replace(context.Background(), "repo", mustLexicalCorpus(t, chunks)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	searcher, err := lexical.NewSearcher(store)
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}

	hits, err := searcher.Search(context.Background(), search.Query{
		RepositoryID: "repo",
		Text:         "search",
		Limit:        10,
		Filter: search.Filter{
			PathPrefixes: []string{"internal/"},
			Languages:    []source.Language{source.LanguagePython, source.LanguageTypeScript},
		},
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := hitIDs(hits); len(got) != 2 || got[0] != "python-internal" || got[1] != "typescript-internal" {
		t.Fatalf("Search() IDs = %v, want [python-internal typescript-internal]", got)
	}
}

func TestSearcherFiltersRejectInvalidValuesBeforeLoading(t *testing.T) {
	store := newStore(t)
	searcher, err := lexical.NewSearcher(store)
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}

	tests := []struct {
		name   string
		filter search.Filter
	}{
		{name: "empty path prefix", filter: search.Filter{PathPrefixes: []string{""}}},
		{name: "absolute path prefix", filter: search.Filter{PathPrefixes: []string{"/internal"}}},
		{name: "parent traversal", filter: search.Filter{PathPrefixes: []string{"../internal"}}},
		{name: "non canonical path", filter: search.Filter{PathPrefixes: []string{"internal/../pkg"}}},
		{name: "repeated separator", filter: search.Filter{PathPrefixes: []string{"internal//search"}}},
		{name: "backslash", filter: search.Filter{PathPrefixes: []string{`internal\search`}}},
		{name: "unknown language", filter: search.Filter{Languages: []source.Language{source.LanguageUnknown}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := searcher.Search(context.Background(), search.Query{
				RepositoryID: "repo",
				Text:         "search",
				Limit:        10,
				Filter:       tt.filter,
			})
			if !errors.Is(err, lexical.ErrInvalidQuery) || !errors.Is(err, search.ErrInvalidFilter) {
				t.Fatalf("Search() error = %v, want ErrInvalidQuery wrapping ErrInvalidFilter", err)
			}
		})
	}
}

func TestSearcherSplitsCamelCaseAndSnakeCase(t *testing.T) {
	store := newStore(t)
	chunks := []source.Chunk{
		chunk("camel", "src/service.ts", 1, "loadUserProfile", "export function loadUserProfile() {}"),
		chunk("snake", "src/load_user.py", 1, "other", "VALUE = true"),
	}
	if err := store.Replace(context.Background(), "repo", mustLexicalCorpus(t, chunks)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	searcher, err := lexical.NewSearcher(store)
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}

	hits, err := searcher.Search(context.Background(), search.Query{
		RepositoryID: "repo",
		Text:         "LOAD user",
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(hits) != 2 || hits[0].Chunk.ID != "camel" || hits[1].Chunk.ID != "snake" {
		t.Fatalf("Search() IDs = %v, want [camel snake]", hitIDs(hits))
	}
}

func TestSearcherUsesDeterministicTieBreakersAndLimit(t *testing.T) {
	store := newStore(t)
	chunks := []source.Chunk{
		chunk("b", "b.py", 1, "", "token"),
		chunk("a-late", "a.py", 8, "", "token"),
		chunk("a-first", "a.py", 2, "", "token"),
	}
	if err := store.Replace(context.Background(), "repo", mustLexicalCorpus(t, chunks)); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	searcher, err := lexical.NewSearcher(store)
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}

	hits, err := searcher.Search(context.Background(), search.Query{
		RepositoryID: "repo",
		Text:         "token",
		Limit:        2,
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if got := hitIDs(hits); len(got) != 2 || got[0] != "a-first" || got[1] != "a-late" {
		t.Fatalf("Search() IDs = %v, want [a-first a-late]", got)
	}
}

func TestSearcherRejectsInvalidQueries(t *testing.T) {
	store := newStore(t)
	searcher, err := lexical.NewSearcher(store)
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}

	tests := []search.Query{
		{Text: "term", Limit: 1},
		{RepositoryID: "  ", Text: "term", Limit: 1},
		{RepositoryID: "repo", Text: "  ", Limit: 1},
		{RepositoryID: "repo", Text: "term", Limit: 0},
	}
	for _, query := range tests {
		if _, err := searcher.Search(context.Background(), query); !errors.Is(err, lexical.ErrInvalidQuery) {
			t.Errorf("Search(%#v) error = %v, want ErrInvalidQuery", query, err)
		}
	}

	if _, err := lexical.NewSearcher(nil); !errors.Is(err, lexical.ErrInvalidLoader) {
		t.Fatalf("NewSearcher(nil) error = %v, want ErrInvalidLoader", err)
	}
}

func TestSearcherPreservesSnapshotErrorsAndCancellation(t *testing.T) {
	store := newStore(t)
	searcher, err := lexical.NewSearcher(store)
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}

	_, err = searcher.Search(context.Background(), search.Query{RepositoryID: "missing", Text: "term", Limit: 1})
	if !errors.Is(err, localstore.ErrNotFound) {
		t.Fatalf("Search(missing) error = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "search repository snapshot") {
		t.Errorf("Search(missing) error = %q, want operation context", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = searcher.Search(ctx, search.Query{RepositoryID: "repo", Text: "term", Limit: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Search(canceled) error = %v, want context.Canceled", err)
	}
}

func newStore(t *testing.T) *localstore.Store {
	t.Helper()
	store, err := localstore.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	return store
}

func mustLexicalCorpus(t *testing.T, chunks []source.Chunk) source.Corpus {
	t.Helper()
	corpus, err := source.NewCorpus(ingest.ScanPolicyVersion, chunks)
	if err != nil {
		t.Fatalf("NewCorpus() error = %v", err)
	}
	return corpus
}

func chunk(id, path string, startLine int, symbolName, text string) source.Chunk {
	return source.Chunk{
		ID:         id,
		Text:       text,
		Language:   source.LanguagePython,
		SymbolName: symbolName,
		Reference:  source.Reference{Path: path, StartLine: startLine, EndLine: startLine},
	}
}

func assertScore(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("score = %.12f, want %.12f", got, want)
	}
}

func hitIDs(hits []search.Hit) []string {
	ids := make([]string, len(hits))
	for index := range hits {
		ids[index] = hits[index].Chunk.ID
	}
	return ids
}
