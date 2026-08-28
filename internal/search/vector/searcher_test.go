package vector_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	"github.com/ToledoVitor/GoContext/internal/index"
	indexsqlite "github.com/ToledoVitor/GoContext/internal/index/sqlite"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/search/vector"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestNewSearcherRejectsNilAndTypedNilDependencies(t *testing.T) {
	var typedNilEmbedder *fakeEmbedder
	var typedNilIndex *fakeIndex
	nonNilEmbedder := &fakeEmbedder{}
	nonNilIndex := &fakeIndex{}

	tests := []struct {
		name     string
		embedder embedding.Embedder
		index    vector.Index
	}{
		{name: "nil embedder", index: nonNilIndex},
		{name: "typed nil embedder", embedder: typedNilEmbedder, index: nonNilIndex},
		{name: "nil index", embedder: nonNilEmbedder},
		{name: "typed nil index", embedder: nonNilEmbedder, index: typedNilIndex},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := vector.NewSearcher(test.embedder, test.index); !errors.Is(err, vector.ErrInvalidSearcher) {
				t.Fatalf("NewSearcher() error = %v, want ErrInvalidSearcher", err)
			}
		})
	}
}

func TestSearcherRejectsInvalidQueryBeforeDependencies(t *testing.T) {
	embedder := &fakeEmbedder{}
	index := &fakeIndex{}
	searcher, err := vector.NewSearcher(embedder, index)
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}

	invalidQueries := []search.Query{
		{Text: "PRIVATE_QUERY_CANARY", Limit: 1},
		{RepositoryID: "  ", Text: "PRIVATE_QUERY_CANARY", Limit: 1},
		{RepositoryID: "repository", Text: "  ", Limit: 1},
		{RepositoryID: "repository", Text: "PRIVATE_QUERY_CANARY", Limit: 0},
		{RepositoryID: "repository", Text: "PRIVATE_QUERY_CANARY", Limit: -1},
		{RepositoryID: "repository", Text: "PRIVATE_QUERY_CANARY", Limit: math.MaxInt32 + 1},
	}
	for _, query := range invalidQueries {
		_, searchErr := searcher.Search(context.Background(), query)
		if !errors.Is(searchErr, vector.ErrInvalidQuery) {
			t.Errorf("Search(%#v) error = %v, want ErrInvalidQuery", query, searchErr)
		}
		if strings.Contains(searchErr.Error(), "PRIVATE_QUERY_CANARY") {
			t.Errorf("Search() error exposes query text: %v", searchErr)
		}
	}

	_, err = searcher.Search(context.Background(), search.Query{
		RepositoryID: "repository",
		Text:         "PRIVATE_QUERY_CANARY",
		Limit:        1,
		Filter:       search.Filter{Languages: []source.Language{source.LanguageUnknown}},
	})
	if !errors.Is(err, vector.ErrInvalidQuery) || !errors.Is(err, search.ErrInvalidFilter) {
		t.Fatalf("Search(invalid filter) error = %v, want ErrInvalidQuery wrapping ErrInvalidFilter", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = searcher.Search(canceled, search.Query{RepositoryID: "repository", Text: "PRIVATE_QUERY_CANARY", Limit: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Search(canceled) error = %v, want context.Canceled", err)
	}
	if _, err := searcher.Search(nil, search.Query{RepositoryID: "repository", Text: "PRIVATE_QUERY_CANARY", Limit: 1}); !errors.Is(err, vector.ErrInvalidQuery) {
		t.Fatalf("Search(nil context) error = %v, want ErrInvalidQuery", err)
	}

	if index.describeCalls != 0 || index.searchCalls != 0 || embedder.profileCalls != 0 || embedder.embedCalls != 0 {
		t.Fatalf("invalid input dependency calls = describe:%d search:%d profile:%d embed:%d, want zero", index.describeCalls, index.searchCalls, embedder.profileCalls, embedder.embedCalls)
	}
}

func TestSearcherPreflightClassifiesIndexErrorsWithoutLeakingCauses(t *testing.T) {
	known := []error{
		context.Canceled,
		context.DeadlineExceeded,
		index.ErrNotFound,
		index.ErrReindexRequired,
		vector.ErrVectorUnavailable,
		vector.ErrVectorIntegrity,
		vector.ErrIncompatibleSpace,
		vector.ErrGenerationChanged,
	}
	for _, sentinel := range known {
		t.Run(sentinel.Error(), func(t *testing.T) {
			embedder := &fakeEmbedder{profile: testProfile()}
			backend := &fakeIndex{describeFn: func(context.Context, string) (vector.Metadata, error) {
				return vector.Metadata{}, fmt.Errorf("PRIVATE_DESCRIBE_CANARY: %w", sentinel)
			}}
			searcher := mustSearcher(t, embedder, backend)

			_, err := searcher.Search(context.Background(), validQuery())
			if !errors.Is(err, sentinel) {
				t.Fatalf("Search() error = %v, want %v", err, sentinel)
			}
			if strings.Contains(err.Error(), "PRIVATE_DESCRIBE_CANARY") {
				t.Fatalf("Search() error exposes Describe cause: %v", err)
			}
			if embedder.profileCalls != 0 || embedder.embedCalls != 0 || backend.searchCalls != 0 {
				t.Fatalf("post-Describe calls = profile:%d embed:%d search:%d, want zero", embedder.profileCalls, embedder.embedCalls, backend.searchCalls)
			}
		})
	}

	embedder := &fakeEmbedder{profile: testProfile()}
	backend := &fakeIndex{describeFn: func(context.Context, string) (vector.Metadata, error) {
		return vector.Metadata{}, errors.New("PRIVATE_UNKNOWN_INDEX_CANARY")
	}}
	_, err := mustSearcher(t, embedder, backend).Search(context.Background(), validQuery())
	if !errors.Is(err, vector.ErrBackend) {
		t.Fatalf("Search(unknown Describe error) = %v, want ErrBackend", err)
	}
	if strings.Contains(err.Error(), "PRIVATE_UNKNOWN_INDEX_CANARY") {
		t.Fatalf("Search() error exposes unknown Describe cause: %v", err)
	}
}

func TestSearcherClassifiesMissingAndStaleIndexesAsVectorUnavailable(t *testing.T) {
	for _, sentinel := range []error{index.ErrNotFound, index.ErrReindexRequired} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			t.Run("Describe", func(t *testing.T) {
				backend := &fakeIndex{describeFn: func(context.Context, string) (vector.Metadata, error) {
					return vector.Metadata{}, fmt.Errorf("PRIVATE_INDEX_CANARY: %w", sentinel)
				}}
				_, err := mustSearcher(t, &fakeEmbedder{profile: testProfile()}, backend).Search(context.Background(), validQuery())
				assertUnavailableIndexError(t, err, sentinel)
			})

			t.Run("Search", func(t *testing.T) {
				metadata := testMetadata()
				backend := &fakeIndex{
					metadata: metadata,
					searchFn: func(context.Context, vector.IndexQuery) ([]vector.Candidate, error) {
						return nil, fmt.Errorf("PRIVATE_INDEX_CANARY: %w", sentinel)
					},
				}
				_, err := mustSearcher(t, validEmbedder(metadata), backend).Search(context.Background(), validQuery())
				assertUnavailableIndexError(t, err, sentinel)
			})
		})
	}
}

func assertUnavailableIndexError(t *testing.T, err, original error) {
	t.Helper()
	if !errors.Is(err, original) || !errors.Is(err, vector.ErrVectorUnavailable) {
		t.Fatalf("Search() error = %v, want both %v and ErrVectorUnavailable", err, original)
	}
	if strings.Contains(err.Error(), "PRIVATE_INDEX_CANARY") {
		t.Fatalf("Search() error exposes index cause: %v", err)
	}
}

func TestSearcherRejectsMalformedOrIncompatibleMetadataBeforeEmbedding(t *testing.T) {
	valid := testMetadata()
	malformed := []struct {
		name   string
		mutate func(*vector.Metadata)
	}{
		{name: "empty generation", mutate: func(m *vector.Metadata) { m.GenerationID = "" }},
		{name: "blank generation", mutate: func(m *vector.Metadata) { m.GenerationID = "  " }},
		{name: "empty corpus revision", mutate: func(m *vector.Metadata) { m.CorpusRevision = "" }},
		{name: "blank fingerprint", mutate: func(m *vector.Metadata) { m.Profile.Fingerprint = "  " }},
		{name: "blank model", mutate: func(m *vector.Metadata) { m.Profile.Model = "  " }},
		{name: "zero dimensions", mutate: func(m *vector.Metadata) { m.Dimensions = 0 }},
		{name: "wrong metric", mutate: func(m *vector.Metadata) { m.Metric = index.VectorMetric("PRIVATE_METRIC_CANARY") }},
	}
	for _, test := range malformed {
		t.Run(test.name, func(t *testing.T) {
			metadata := valid
			test.mutate(&metadata)
			embedder := &fakeEmbedder{profile: testProfile()}
			backend := &fakeIndex{metadata: metadata}

			_, err := mustSearcher(t, embedder, backend).Search(context.Background(), validQuery())
			if !errors.Is(err, vector.ErrVectorIntegrity) {
				t.Fatalf("Search() error = %v, want ErrVectorIntegrity", err)
			}
			if strings.Contains(err.Error(), "PRIVATE_METRIC_CANARY") {
				t.Fatalf("Search() error exposes metadata: %v", err)
			}
			if embedder.profileCalls != 0 || embedder.embedCalls != 0 || backend.searchCalls != 0 {
				t.Fatalf("malformed metadata dependency calls = profile:%d embed:%d search:%d, want zero", embedder.profileCalls, embedder.embedCalls, backend.searchCalls)
			}
		})
	}

	for _, profile := range []embedding.Profile{
		{Fingerprint: "PRIVATE_OTHER_FINGERPRINT", Model: valid.Profile.Model},
		{Fingerprint: valid.Profile.Fingerprint, Model: "PRIVATE_OTHER_MODEL"},
	} {
		embedder := &fakeEmbedder{profile: profile}
		backend := &fakeIndex{metadata: valid}
		_, err := mustSearcher(t, embedder, backend).Search(context.Background(), validQuery())
		if !errors.Is(err, vector.ErrIncompatibleSpace) {
			t.Fatalf("Search(profile mismatch) error = %v, want ErrIncompatibleSpace", err)
		}
		for _, canary := range []string{"PRIVATE_OTHER_FINGERPRINT", "PRIVATE_OTHER_MODEL"} {
			if strings.Contains(err.Error(), canary) {
				t.Fatalf("Search() error exposes profile: %v", err)
			}
		}
		if embedder.profileCalls != 1 || embedder.embedCalls != 0 || backend.searchCalls != 0 {
			t.Fatalf("profile mismatch calls = profile:%d embed:%d search:%d, want 1,0,0", embedder.profileCalls, embedder.embedCalls, backend.searchCalls)
		}
	}
}

func TestSearcherEmbedsOneQueryAndReturnsCanonicalNormalizedHits(t *testing.T) {
	metadata := testMetadata()
	query := validQuery()
	query.Limit = 3
	query.Filter = search.Filter{
		PathPrefixes: []string{"pkg", "internal"},
		Languages:    []source.Language{source.LanguageJavaScript, source.LanguagePython, source.LanguageTypeScript},
	}
	chunks := []source.Chunk{
		testChunk("negative", "pkg/negative.py", 7, 8, "NEGATIVE_SOURCE"),
		testChunk("zero", "pkg/zero.py", 4, 5, "ZERO_SOURCE"),
		testChunk("positive", "pkg/positive.py", 1, 3, "POSITIVE_SOURCE"),
	}
	embedder := &fakeEmbedder{
		profile: metadata.Profile,
		embedFn: func(context.Context, embedding.Purpose, []string) (embedding.Batch, error) {
			return embedding.Batch{
				Profile:    metadata.Profile,
				Dimensions: metadata.Dimensions,
				Vectors:    []embedding.Vector{{3, 4}},
				Requests:   1,
			}, nil
		},
	}
	backend := &fakeIndex{
		metadata: metadata,
		searchFn: func(context.Context, vector.IndexQuery) ([]vector.Candidate, error) {
			return []vector.Candidate{
				{Chunk: chunks[0], Similarity: -1},
				{Chunk: chunks[1], Similarity: 0},
				{Chunk: chunks[2], Similarity: 1},
			}, nil
		},
	}

	hits, err := mustSearcher(t, embedder, backend).Search(context.Background(), query)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if backend.describeCalls != 1 || len(backend.describedRepositories) != 1 || backend.describedRepositories[0] != query.RepositoryID {
		t.Fatalf("Describe calls = %d repositories = %v, want one exact repository", backend.describeCalls, backend.describedRepositories)
	}
	if embedder.profileCalls != 1 || embedder.embedCalls != 1 || len(embedder.purposes) != 1 || embedder.purposes[0] != embedding.PurposeQuery ||
		len(embedder.texts) != 1 || !reflect.DeepEqual(embedder.texts[0], []string{query.Text}) {
		t.Fatalf("embedding calls = profile:%d embed:%d purpose:%v texts:%v, want one exact PurposeQuery call", embedder.profileCalls, embedder.embedCalls, embedder.purposes, embedder.texts)
	}
	if backend.searchCalls != 1 || len(backend.queries) != 1 {
		t.Fatalf("Index.Search calls = %d queries = %d, want one", backend.searchCalls, len(backend.queries))
	}
	wantIndexQuery := vector.IndexQuery{
		RepositoryID: query.RepositoryID,
		GenerationID: metadata.GenerationID,
		Profile:      metadata.Profile,
		Dimensions:   metadata.Dimensions,
		Metric:       metadata.Metric,
		Vector:       embedding.Vector{3, 4},
		Filter:       query.Filter,
		Limit:        query.Limit,
	}
	if !reflect.DeepEqual(backend.queries[0], wantIndexQuery) {
		t.Fatalf("Index.Search query = %#v, want exact %#v", backend.queries[0], wantIndexQuery)
	}

	wantIDs := []string{"positive", "zero", "negative"}
	wantScores := []float64{1, 0.5, 0}
	chunkByID := map[string]source.Chunk{
		"negative": chunks[0],
		"zero":     chunks[1],
		"positive": chunks[2],
	}
	if len(hits) != len(wantIDs) {
		t.Fatalf("Search() hits = %#v, want %d", hits, len(wantIDs))
	}
	for position, hit := range hits {
		if hit.Chunk.ID != wantIDs[position] || hit.Score != wantScores[position] {
			t.Errorf("hit %d = %#v, want ID %q score %v", position, hit, wantIDs[position], wantScores[position])
		}
		if !reflect.DeepEqual(hit.Chunk, chunkByID[hit.Chunk.ID]) {
			t.Errorf("hit %d chunk = %#v, want byte-identical canonical %#v", position, hit.Chunk, chunkByID[hit.Chunk.ID])
		}
	}
}

func TestSearcherClassifiesEmbeddingFailuresWithoutLeakingCauses(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "temporary unavailable", err: embedding.ErrSemanticUnavailable, want: embedding.ErrSemanticUnavailable},
		{name: "invalid batch", err: embedding.ErrInvalidBatch, want: vector.ErrInvalidQueryVector},
		{name: "invalid vector", err: embedding.ErrInvalidVector, want: vector.ErrInvalidQueryVector},
		{name: "invalid vector wins over temporary", err: errors.Join(embedding.ErrSemanticUnavailable, embedding.ErrInvalidVector), want: vector.ErrInvalidQueryVector},
		{name: "canceled", err: context.Canceled, want: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded, want: context.DeadlineExceeded},
		{name: "unknown", err: errors.New("unknown"), want: vector.ErrBackend},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			embedder := &fakeEmbedder{
				profile: testProfile(),
				embedFn: func(context.Context, embedding.Purpose, []string) (embedding.Batch, error) {
					return embedding.Batch{}, fmt.Errorf("PRIVATE_EMBED_CANARY: %w", test.err)
				},
			}
			backend := &fakeIndex{metadata: testMetadata()}

			_, err := mustSearcher(t, embedder, backend).Search(context.Background(), validQuery())
			if !errors.Is(err, test.want) {
				t.Fatalf("Search() error = %v, want %v", err, test.want)
			}
			if strings.Contains(err.Error(), "PRIVATE_EMBED_CANARY") {
				t.Fatalf("Search() error exposes embedding cause: %v", err)
			}
			if embedder.embedCalls != 1 || backend.searchCalls != 0 {
				t.Fatalf("failure calls = embed:%d search:%d, want 1,0", embedder.embedCalls, backend.searchCalls)
			}
		})
	}
}

func TestSearcherRejectsMalformedEmbeddingBatchesBeforeIndexSearch(t *testing.T) {
	validBatch := embedding.Batch{
		Profile:    testProfile(),
		Dimensions: 2,
		Vectors:    []embedding.Vector{{1, 0}},
		Requests:   1,
	}
	tests := []struct {
		name   string
		mutate func(*embedding.Batch)
	}{
		{name: "profile fingerprint", mutate: func(b *embedding.Batch) { b.Profile.Fingerprint = "PRIVATE_OTHER_FINGERPRINT" }},
		{name: "profile model", mutate: func(b *embedding.Batch) { b.Profile.Model = "PRIVATE_OTHER_MODEL" }},
		{name: "dimensions metadata", mutate: func(b *embedding.Batch) { b.Dimensions = 1; b.Vectors[0] = embedding.Vector{1} }},
		{name: "count", mutate: func(b *embedding.Batch) { b.Vectors = append(b.Vectors, embedding.Vector{0, 1}) }},
		{name: "component NaN", mutate: func(b *embedding.Batch) { b.Vectors[0][0] = float32(math.NaN()) }},
		{name: "component infinity", mutate: func(b *embedding.Batch) { b.Vectors[0][0] = float32(math.Inf(1)) }},
		{name: "zero vector", mutate: func(b *embedding.Batch) { b.Vectors[0] = embedding.Vector{0, 0} }},
		{name: "missing requests", mutate: func(b *embedding.Batch) { b.Requests = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			batch := cloneBatch(validBatch)
			test.mutate(&batch)
			embedder := &fakeEmbedder{
				profile: testProfile(),
				embedFn: func(context.Context, embedding.Purpose, []string) (embedding.Batch, error) { return batch, nil },
			}
			backend := &fakeIndex{metadata: testMetadata()}

			_, err := mustSearcher(t, embedder, backend).Search(context.Background(), validQuery())
			if !errors.Is(err, vector.ErrInvalidQueryVector) {
				t.Fatalf("Search() error = %v, want ErrInvalidQueryVector", err)
			}
			if backend.searchCalls != 0 {
				t.Fatalf("Index.Search calls = %d, want zero", backend.searchCalls)
			}
			for _, canary := range []string{"PRIVATE_OTHER_FINGERPRINT", "PRIVATE_OTHER_MODEL"} {
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("Search() error exposes batch metadata: %v", err)
				}
			}
		})
	}
}

func TestSearcherParentCancellationWinsAtEmbeddingBoundary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	embedder := &fakeEmbedder{
		profile: testProfile(),
		embedFn: func(context.Context, embedding.Purpose, []string) (embedding.Batch, error) {
			cancel()
			return embedding.Batch{}, errors.New("PRIVATE_EMBED_CANCEL_CANARY")
		},
	}
	backend := &fakeIndex{metadata: testMetadata()}

	_, err := mustSearcher(t, embedder, backend).Search(ctx, validQuery())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Search() error = %v, want parent context.Canceled", err)
	}
	if strings.Contains(err.Error(), "PRIVATE_EMBED_CANCEL_CANARY") {
		t.Fatalf("Search() error exposes embedder cause after parent cancellation: %v", err)
	}
	if backend.searchCalls != 0 {
		t.Fatalf("Index.Search calls = %d, want zero", backend.searchCalls)
	}
}

func TestSearcherCancellationInterruptsEmbeddingValidation(t *testing.T) {
	const dimensions = 1 << 20
	values := make(embedding.Vector, dimensions)
	values[0] = 1
	embedder := &fakeEmbedder{
		profile: testProfile(),
		embedFn: func(context.Context, embedding.Purpose, []string) (embedding.Batch, error) {
			return embedding.Batch{
				Profile:    testProfile(),
				Dimensions: dimensions,
				Vectors:    []embedding.Vector{values},
				Requests:   1,
			}, nil
		},
	}
	backend := &fakeIndex{metadata: vector.Metadata{
		GenerationID:   "generation",
		CorpusRevision: "revision",
		Profile:        testProfile(),
		Dimensions:     dimensions,
		Metric:         index.VectorMetricCosine,
	}}
	ctx := newCancelOnErrContext(32)

	_, err := mustSearcher(t, embedder, backend).Search(ctx, validQuery())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Search() error = %v, want context.Canceled", err)
	}
	if backend.searchCalls != 0 {
		t.Fatalf("Index.Search calls = %d, want zero after canceled validation", backend.searchCalls)
	}
}

func TestSearcherDoesNotRetryWhenGenerationChangesAtIndexSearch(t *testing.T) {
	embedder := validEmbedder(testMetadata())
	backend := &fakeIndex{
		metadata: testMetadata(),
		searchFn: func(context.Context, vector.IndexQuery) ([]vector.Candidate, error) {
			return nil, fmt.Errorf("PRIVATE_GENERATION_CANARY: %w", vector.ErrGenerationChanged)
		},
	}

	_, err := mustSearcher(t, embedder, backend).Search(context.Background(), validQuery())
	if !errors.Is(err, vector.ErrGenerationChanged) {
		t.Fatalf("Search() error = %v, want ErrGenerationChanged", err)
	}
	if strings.Contains(err.Error(), "PRIVATE_GENERATION_CANARY") {
		t.Fatalf("Search() error exposes generation backend cause: %v", err)
	}
	if backend.describeCalls != 1 || backend.searchCalls != 1 || embedder.embedCalls != 1 {
		t.Fatalf("generation change calls = describe:%d search:%d embed:%d, want 1,1,1 with no retry", backend.describeCalls, backend.searchCalls, embedder.embedCalls)
	}
}

func TestSearcherRejectsInvalidCandidateTailBeforeApplyingLimit(t *testing.T) {
	valid := vector.Candidate{Chunk: testChunk("valid", "pkg/valid.py", 1, 2, "VALID_SOURCE"), Similarity: 1}
	tests := []struct {
		name   string
		mutate func(*vector.Candidate)
	}{
		{name: "duplicate ID", mutate: func(c *vector.Candidate) { c.Chunk.ID = valid.Chunk.ID }},
		{name: "empty ID", mutate: func(c *vector.Candidate) { c.Chunk.ID = "" }},
		{name: "empty text", mutate: func(c *vector.Candidate) { c.Chunk.Text = "" }},
		{name: "invalid reference", mutate: func(c *vector.Candidate) { c.Chunk.Reference.Path = "pkg/../PRIVATE_PATH_CANARY" }},
		{name: "NaN similarity", mutate: func(c *vector.Candidate) { c.Similarity = math.NaN() }},
		{name: "positive infinity", mutate: func(c *vector.Candidate) { c.Similarity = math.Inf(1) }},
		{name: "negative infinity", mutate: func(c *vector.Candidate) { c.Similarity = math.Inf(-1) }},
		{name: "above one", mutate: func(c *vector.Candidate) { c.Similarity = 1.000001 }},
		{name: "below minus one", mutate: func(c *vector.Candidate) { c.Similarity = -1.000001 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bad := vector.Candidate{Chunk: testChunk("bad", "pkg/bad.py", 3, 4, "PRIVATE_BAD_SOURCE_CANARY"), Similarity: -1}
			test.mutate(&bad)
			metadata := testMetadata()
			backend := &fakeIndex{
				metadata: metadata,
				searchFn: func(context.Context, vector.IndexQuery) ([]vector.Candidate, error) {
					return []vector.Candidate{valid, bad}, nil
				},
			}
			query := validQuery()
			query.Limit = 1

			hits, err := mustSearcher(t, validEmbedder(metadata), backend).Search(context.Background(), query)
			if !errors.Is(err, vector.ErrVectorIntegrity) {
				t.Fatalf("Search() hits = %#v error = %v, want ErrVectorIntegrity from tail", hits, err)
			}
			for _, canary := range []string{"PRIVATE_BAD_SOURCE_CANARY", "PRIVATE_PATH_CANARY"} {
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("Search() error exposes candidate data: %v", err)
				}
			}
		})
	}
}

func TestSearcherUsesAllTieBreakersBeforeExactLimit(t *testing.T) {
	metadata := testMetadata()
	candidates := []vector.Candidate{
		{Chunk: testChunk("path-b", "b.py", 1, 2, "PATH_B"), Similarity: 0},
		{Chunk: testChunk("line-late", "a.py", 2, 2, "LINE_LATE"), Similarity: 0},
		{Chunk: testChunk("end-late", "a.py", 1, 3, "END_LATE"), Similarity: 0},
		{Chunk: testChunk("id-b", "a.py", 1, 2, "ID_B"), Similarity: 0},
		{Chunk: testChunk("id-a", "a.py", 1, 2, "ID_A"), Similarity: 0},
	}
	backend := &fakeIndex{metadata: metadata, searchFn: func(context.Context, vector.IndexQuery) ([]vector.Candidate, error) {
		return candidates, nil
	}}
	query := validQuery()
	query.Limit = 4

	hits, err := mustSearcher(t, validEmbedder(metadata), backend).Search(context.Background(), query)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	want := []string{"id-a", "id-b", "end-late", "line-late"}
	if len(hits) != len(want) {
		t.Fatalf("Search() hits = %#v, want exact limit %d", hits, len(want))
	}
	for position := range hits {
		if hits[position].Chunk.ID != want[position] || hits[position].Score != 0.5 {
			t.Fatalf("Search() hit %d = %#v, want ID %q score 0.5", position, hits[position], want[position])
		}
	}
}

func TestSearcherDefensivelyCopiesProviderAndCallerSlices(t *testing.T) {
	metadata := testMetadata()
	providerVector := embedding.Vector{1, 0}
	query := validQuery()
	query.Filter = search.Filter{
		PathPrefixes: []string{"pkg"},
		Languages:    []source.Language{source.LanguagePython},
	}
	wantQuery := query
	wantQuery.Filter.PathPrefixes = append([]string(nil), query.Filter.PathPrefixes...)
	wantQuery.Filter.Languages = append([]source.Language(nil), query.Filter.Languages...)
	embedder := &fakeEmbedder{
		profile: metadata.Profile,
		embedFn: func(context.Context, embedding.Purpose, []string) (embedding.Batch, error) {
			return embedding.Batch{Profile: metadata.Profile, Dimensions: 2, Vectors: []embedding.Vector{providerVector}, Requests: 1}, nil
		},
	}
	backend := &fakeIndex{
		metadata: metadata,
		searchFn: func(_ context.Context, indexQuery vector.IndexQuery) ([]vector.Candidate, error) {
			indexQuery.Vector[0] = -1
			indexQuery.Filter.PathPrefixes[0] = "PRIVATE_MUTATED_PATH"
			indexQuery.Filter.Languages[0] = source.LanguageTypeScript
			return []vector.Candidate{{Chunk: testChunk("canonical", "pkg/canonical.py", 1, 2, "CANONICAL_SOURCE"), Similarity: 1}}, nil
		},
	}

	hits, err := mustSearcher(t, embedder, backend).Search(context.Background(), query)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if !reflect.DeepEqual(query, wantQuery) {
		t.Fatalf("Search() mutated caller query = %#v, want %#v", query, wantQuery)
	}
	if !reflect.DeepEqual(providerVector, embedding.Vector{1, 0}) {
		t.Fatalf("Search() let index mutate provider vector = %#v, want [1 0]", providerVector)
	}
	if len(hits) != 1 || hits[0].Chunk.ID != "canonical" || hits[0].Score != 1 {
		t.Fatalf("Search() hits = %#v, want stable canonical hit", hits)
	}
}

func TestSearcherClassifiesIndexSearchErrorsWithoutLeakingCauses(t *testing.T) {
	known := []error{
		context.Canceled,
		context.DeadlineExceeded,
		index.ErrNotFound,
		index.ErrReindexRequired,
		vector.ErrVectorUnavailable,
		vector.ErrVectorIntegrity,
		vector.ErrIncompatibleSpace,
		vector.ErrGenerationChanged,
	}
	for _, sentinel := range known {
		t.Run(sentinel.Error(), func(t *testing.T) {
			metadata := testMetadata()
			backend := &fakeIndex{
				metadata: metadata,
				searchFn: func(context.Context, vector.IndexQuery) ([]vector.Candidate, error) {
					return nil, fmt.Errorf("PRIVATE_SEARCH_CANARY: %w", sentinel)
				},
			}
			_, err := mustSearcher(t, validEmbedder(metadata), backend).Search(context.Background(), validQuery())
			if !errors.Is(err, sentinel) {
				t.Fatalf("Search() error = %v, want %v", err, sentinel)
			}
			if strings.Contains(err.Error(), "PRIVATE_SEARCH_CANARY") {
				t.Fatalf("Search() error exposes Index.Search cause: %v", err)
			}
		})
	}

	t.Run("integrity wins over unavailable", func(t *testing.T) {
		metadata := testMetadata()
		backend := &fakeIndex{
			metadata: metadata,
			searchFn: func(context.Context, vector.IndexQuery) ([]vector.Candidate, error) {
				return nil, errors.Join(vector.ErrVectorUnavailable, vector.ErrVectorIntegrity)
			},
		}
		_, err := mustSearcher(t, validEmbedder(metadata), backend).Search(context.Background(), validQuery())
		if !errors.Is(err, vector.ErrVectorIntegrity) || errors.Is(err, vector.ErrVectorUnavailable) {
			t.Fatalf("Search(joined backend error) = %v, want only ErrVectorIntegrity", err)
		}
	})

	metadata := testMetadata()
	backend := &fakeIndex{
		metadata: metadata,
		searchFn: func(context.Context, vector.IndexQuery) ([]vector.Candidate, error) {
			return nil, errors.New("PRIVATE_UNKNOWN_SEARCH_CANARY")
		},
	}
	_, err := mustSearcher(t, validEmbedder(metadata), backend).Search(context.Background(), validQuery())
	if !errors.Is(err, vector.ErrBackend) {
		t.Fatalf("Search(unknown backend error) = %v, want ErrBackend", err)
	}
	if strings.Contains(err.Error(), "PRIVATE_UNKNOWN_SEARCH_CANARY") {
		t.Fatalf("Search() error exposes unknown Index.Search cause: %v", err)
	}
}

func TestSearcherCancellationInterruptsCandidateValidationAndSorting(t *testing.T) {
	const candidateCount = 1024
	candidates := make([]vector.Candidate, candidateCount)
	for position := range candidates {
		id := fmt.Sprintf("chunk-%04d", position)
		candidates[position] = vector.Candidate{
			Chunk:      testChunk(id, fmt.Sprintf("pkg/%04d.py", candidateCount-position), position+1, position+1, "SOURCE"),
			Similarity: float64(position%3-1) / 2,
		}
	}

	tests := []struct {
		name     string
		cancelAt int
	}{
		{name: "candidate validation", cancelAt: 12},
		{name: "deterministic sort", cancelAt: 20},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := testMetadata()
			backend := &fakeIndex{metadata: metadata, searchFn: func(context.Context, vector.IndexQuery) ([]vector.Candidate, error) {
				return candidates, nil
			}}
			query := validQuery()
			query.Limit = candidateCount
			ctx := newCancelOnErrContext(test.cancelAt)

			hits, err := mustSearcher(t, validEmbedder(metadata), backend).Search(ctx, query)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Search() hits = %d error = %v, want context.Canceled", len(hits), err)
			}
			if hits != nil {
				t.Fatalf("Search() hits = %#v, want nil on cancellation", hits)
			}
			if backend.searchCalls != 1 {
				t.Fatalf("Index.Search calls = %d, want one without retry", backend.searchCalls)
			}
		})
	}
}

func TestSearcherSQLiteReaderStaysPinnedAcrossManifestSwap(t *testing.T) {
	store, err := indexsqlite.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	firstChunk := testChunk("old", "old.py", 1, 2, "OLD_CANONICAL_SOURCE")
	first := testGeneration(t, "repository", "generation-a", "", []source.Chunk{firstChunk}, []index.VectorRecord{{ChunkID: firstChunk.ID, Values: embedding.Vector{1, 0}}})
	if err := store.Replace(context.Background(), first); err != nil {
		t.Fatalf("Replace(first) error = %v", err)
	}
	reader, err := store.BindActive(context.Background(), first.RepositoryID)
	if err != nil {
		t.Fatalf("BindActive() error = %v", err)
	}
	readerClosed := false
	t.Cleanup(func() {
		if !readerClosed {
			if err := reader.Close(); err != nil {
				t.Errorf("BoundReader.Close() error = %v", err)
			}
		}
	})
	secondChunk := testChunk("new", "new.py", 1, 2, "NEW_CANONICAL_SOURCE")
	second := testGeneration(t, first.RepositoryID, "generation-b", first.ID, []source.Chunk{secondChunk}, []index.VectorRecord{{ChunkID: secondChunk.ID, Values: embedding.Vector{0, 1}}})
	published := false
	embedder := &fakeEmbedder{
		profile: *first.Profile,
		embedFn: func(ctx context.Context, purpose embedding.Purpose, texts []string) (embedding.Batch, error) {
			publishErr := store.Replace(ctx, second)
			var committed *index.CommittedCleanupError
			if publishErr != nil && (!errors.As(publishErr, &committed) || !committed.Published()) {
				t.Fatalf("Replace(second) error = %v, want committed publication while reader is pinned", publishErr)
			}
			published = true
			return embedding.Batch{Profile: *first.Profile, Dimensions: first.Dimensions, Vectors: []embedding.Vector{{1, 0}}, Requests: 1}, nil
		},
	}

	hits, err := mustSearcher(t, embedder, reader).Search(context.Background(), search.Query{RepositoryID: first.RepositoryID, Text: "query", Limit: 1})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if !published || len(hits) != 1 || !reflect.DeepEqual(hits[0].Chunk, firstChunk) || hits[0].Score != 1 {
		t.Fatalf("Search() published = %v hits = %#v, want pinned generation-a canonical hit", published, hits)
	}
	active, err := store.ActiveGeneration(context.Background(), first.RepositoryID)
	if err != nil || active != second.ID {
		t.Fatalf("ActiveGeneration() = %q, %v; want manifest generation-b", active, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("BoundReader.Close() error = %v", err)
	}
	readerClosed = true
	if err := store.Replace(context.Background(), second); err != nil {
		t.Fatalf("Replace(cleanup retry) error = %v", err)
	}
}

type fakeEmbedder struct {
	profileCalls int
	embedCalls   int
	profile      embedding.Profile
	embedFn      func(context.Context, embedding.Purpose, []string) (embedding.Batch, error)
	purposes     []embedding.Purpose
	texts        [][]string
}

func (e *fakeEmbedder) Profile() embedding.Profile {
	e.profileCalls++
	return e.profile
}

func (e *fakeEmbedder) Embed(ctx context.Context, purpose embedding.Purpose, texts []string) (embedding.Batch, error) {
	e.embedCalls++
	e.purposes = append(e.purposes, purpose)
	e.texts = append(e.texts, append([]string(nil), texts...))
	if e.embedFn != nil {
		return e.embedFn(ctx, purpose, texts)
	}
	return embedding.Batch{}, nil
}

type fakeIndex struct {
	describeCalls         int
	searchCalls           int
	metadata              vector.Metadata
	describeFn            func(context.Context, string) (vector.Metadata, error)
	searchFn              func(context.Context, vector.IndexQuery) ([]vector.Candidate, error)
	describedRepositories []string
	queries               []vector.IndexQuery
}

func (i *fakeIndex) Describe(ctx context.Context, repositoryID string) (vector.Metadata, error) {
	i.describeCalls++
	i.describedRepositories = append(i.describedRepositories, repositoryID)
	if i.describeFn != nil {
		return i.describeFn(ctx, repositoryID)
	}
	return i.metadata, nil
}

func (i *fakeIndex) Search(ctx context.Context, query vector.IndexQuery) ([]vector.Candidate, error) {
	i.searchCalls++
	i.queries = append(i.queries, cloneIndexQuery(query))
	if i.searchFn != nil {
		return i.searchFn(ctx, query)
	}
	return nil, nil
}

func mustSearcher(t *testing.T, embedder embedding.Embedder, backend vector.Index) *vector.Searcher {
	t.Helper()
	searcher, err := vector.NewSearcher(embedder, backend)
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}
	return searcher
}

func testProfile() embedding.Profile {
	return embedding.Profile{Fingerprint: "profile-fingerprint", Model: "profile-model"}
}

func testMetadata() vector.Metadata {
	return vector.Metadata{
		GenerationID:   "generation",
		CorpusRevision: "corpus-revision",
		Profile:        testProfile(),
		Dimensions:     2,
		Metric:         index.VectorMetricCosine,
	}
}

func validEmbedder(metadata vector.Metadata) *fakeEmbedder {
	return &fakeEmbedder{
		profile: metadata.Profile,
		embedFn: func(context.Context, embedding.Purpose, []string) (embedding.Batch, error) {
			return embedding.Batch{
				Profile:    metadata.Profile,
				Dimensions: metadata.Dimensions,
				Vectors:    []embedding.Vector{{1, 0}},
				Requests:   1,
			}, nil
		},
	}
}

func validQuery() search.Query {
	return search.Query{RepositoryID: "repository", Text: "query text", Limit: 3}
}

func testChunk(id, path string, startLine, endLine int, text string) source.Chunk {
	return source.Chunk{
		ID:         id,
		Text:       text,
		Language:   source.LanguagePython,
		SymbolName: "Symbol",
		Reference: source.Reference{
			Path:      path,
			StartLine: startLine,
			EndLine:   endLine,
		},
	}
}

func cloneIndexQuery(query vector.IndexQuery) vector.IndexQuery {
	clone := query
	clone.Vector = append(embedding.Vector(nil), query.Vector...)
	clone.Filter.PathPrefixes = append([]string(nil), query.Filter.PathPrefixes...)
	clone.Filter.Languages = append([]source.Language(nil), query.Filter.Languages...)
	return clone
}

func cloneBatch(batch embedding.Batch) embedding.Batch {
	clone := batch
	clone.Vectors = make([]embedding.Vector, len(batch.Vectors))
	for position, values := range batch.Vectors {
		clone.Vectors[position] = append(embedding.Vector(nil), values...)
	}
	return clone
}

func testGeneration(t *testing.T, repositoryID, generationID, baseGeneration string, chunks []source.Chunk, vectors []index.VectorRecord) index.Generation {
	t.Helper()
	corpus, err := source.NewCorpus(ingest.ScanPolicyVersion, chunks)
	if err != nil {
		t.Fatalf("NewCorpus() error = %v", err)
	}
	profile := testProfile()
	return index.Generation{
		RepositoryID:      repositoryID,
		ID:                generationID,
		BaseGeneration:    baseGeneration,
		CorpusRevision:    corpus.Revision,
		ScanPolicyVersion: corpus.PolicyVersion,
		Chunks:            corpus.Chunks,
		Profile:           &profile,
		Dimensions:        2,
		Metric:            index.VectorMetricCosine,
		Vectors:           vectors,
	}
}

type cancelOnErrContext struct {
	context.Context
	cancelAt int
	calls    int
	done     chan struct{}
	once     sync.Once
}

func newCancelOnErrContext(cancelAt int) *cancelOnErrContext {
	return &cancelOnErrContext{Context: context.Background(), cancelAt: cancelAt, done: make(chan struct{})}
}

func (c *cancelOnErrContext) Done() <-chan struct{} {
	return c.done
}

func (c *cancelOnErrContext) Err() error {
	c.calls++
	if c.calls < c.cancelAt {
		return nil
	}
	c.once.Do(func() { close(c.done) })
	return context.Canceled
}
