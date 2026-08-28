package hybrid_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	"github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/search/hybrid"
	"github.com/ToledoVitor/GoContext/internal/search/vector"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestNewSearcherRejectsInvalidDependenciesAndConfig(t *testing.T) {
	var typedNilSearcher *fakeSearcher
	var typedNilObserver *recordingObserver
	nonNilSearcher := &fakeSearcher{}

	tests := []struct {
		name    string
		lexical search.Searcher
		config  hybrid.Config
		want    error
	}{
		{name: "nil lexical", want: hybrid.ErrInvalidSearcher},
		{name: "typed nil lexical", lexical: typedNilSearcher, want: hybrid.ErrInvalidSearcher},
		{name: "unknown mode", lexical: nonNilSearcher, config: hybrid.Config{Mode: "PRIVATE_MODE_CANARY"}, want: hybrid.ErrInvalidConfig},
		{name: "negative RRF k", lexical: nonNilSearcher, config: hybrid.Config{RRFK: -1}, want: hybrid.ErrInvalidConfig},
		{name: "negative lexical weight", lexical: nonNilSearcher, config: hybrid.Config{LexicalWeight: -1}, want: hybrid.ErrInvalidConfig},
		{name: "NaN vector weight", lexical: nonNilSearcher, config: hybrid.Config{VectorWeight: math.NaN()}, want: hybrid.ErrInvalidConfig},
		{name: "infinite vector weight", lexical: nonNilSearcher, config: hybrid.Config{VectorWeight: math.Inf(1)}, want: hybrid.ErrInvalidConfig},
		{name: "negative multiplier", lexical: nonNilSearcher, config: hybrid.Config{CandidateMultiplier: -1}, want: hybrid.ErrInvalidConfig},
		{name: "negative timeout", lexical: nonNilSearcher, config: hybrid.Config{SemanticTimeout: -time.Second}, want: hybrid.ErrInvalidConfig},
		{name: "unbounded timeout", lexical: nonNilSearcher, config: hybrid.Config{SemanticTimeout: 24 * time.Hour}, want: hybrid.ErrInvalidConfig},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := hybrid.NewSearcher(test.lexical, nil, nil, test.config)
			if !errors.Is(err, test.want) {
				t.Fatalf("NewSearcher() error = %v, want %v", err, test.want)
			}
		})
	}

	for _, test := range []struct {
		name     string
		vector   search.Searcher
		observer hybrid.Observer
	}{
		{name: "zero-value defaults"},
		{name: "typed nil vector", vector: typedNilSearcher},
		{name: "typed nil observer", observer: typedNilObserver},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := hybrid.NewSearcher(nonNilSearcher, test.vector, test.observer, hybrid.Config{}); err != nil {
				t.Fatalf("NewSearcher() error = %v, want nil", err)
			}
		})
	}
}

func TestNewSearcherRejectsWeightsThatCannotProducePositiveNormalizedScores(t *testing.T) {
	lexical := &fakeSearcher{}
	tests := []struct {
		name   string
		config hybrid.Config
	}{
		{name: "smallest lexical weight", config: hybrid.Config{LexicalWeight: math.SmallestNonzeroFloat64}},
		{name: "smallest vector weight", config: hybrid.Config{VectorWeight: math.SmallestNonzeroFloat64}},
		{name: "extreme ratio underflows normalization", config: hybrid.Config{LexicalWeight: math.MaxFloat64, VectorWeight: 1e-300}},
		{name: "large k underflows lexical contribution", config: hybrid.Config{RRFK: math.MaxInt, LexicalWeight: math.SmallestNonzeroFloat64}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := hybrid.NewSearcher(lexical, nil, nil, test.config); !errors.Is(err, hybrid.ErrInvalidConfig) {
				t.Fatalf("NewSearcher() error = %v, want ErrInvalidConfig", err)
			}
		})
	}

	t.Run("normal defaults and safe large k remain usable", func(t *testing.T) {
		searcher, err := hybrid.NewSearcher(
			&fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
				return []search.Hit{{Chunk: testChunk("lexical", "lexical.py", 1, 2, "LEXICAL_SOURCE"), Score: 1}}, nil
			}},
			&fakeSearcher{},
			nil,
			hybrid.Config{Mode: hybrid.SemanticPreferred, RRFK: math.MaxInt},
		)
		if err != nil {
			t.Fatalf("NewSearcher() error = %v", err)
		}
		hits, err := searcher.Search(context.Background(), validQuery())
		if err != nil {
			t.Fatalf("Search() error = %v", err)
		}
		if len(hits) != 1 || hits[0].Score != 0.5 {
			t.Fatalf("Search() hits = %#v, want stable default-weight score 0.5", hits)
		}
	})
}

func TestSearcherHandlesAbsentVectorBySemanticMode(t *testing.T) {
	query := validQuery()
	query.Filter.PathPrefixes = []string{"internal"}
	wantQuery := cloneQuery(query)
	lexicalHits := []search.Hit{{Chunk: testChunk("lexical", "internal/lexical.py", 1, 2, "LEXICAL_SOURCE"), Score: 0.73}}

	t.Run("preferred falls back observably", func(t *testing.T) {
		lexical := &fakeSearcher{searchFn: func(_ context.Context, got search.Query) ([]search.Hit, error) {
			if !reflect.DeepEqual(got, wantQuery) {
				t.Fatalf("lexical query = %#v, want original %#v", got, wantQuery)
			}
			return lexicalHits, nil
		}}
		observer := &recordingObserver{}
		searcher, err := hybrid.NewSearcher(lexical, nil, observer, hybrid.Config{Mode: hybrid.SemanticPreferred})
		if err != nil {
			t.Fatalf("NewSearcher() error = %v", err)
		}

		hits, err := searcher.Search(context.Background(), query)
		if err != nil {
			t.Fatalf("Search() error = %v", err)
		}
		if !reflect.DeepEqual(hits, lexicalHits) {
			t.Fatalf("Search() hits = %#v, want lexical fallback %#v", hits, lexicalHits)
		}
		events := observer.snapshot()
		if len(events) != 1 {
			t.Fatalf("observer events = %#v, want exactly one", events)
		}
		if events[0].Backend != "vector" || events[0].Kind != "fallback" || events[0].Reason != "vector_unavailable" || events[0].Latency < 0 {
			t.Fatalf("observer event = %#v, want sanitized vector fallback", events[0])
		}
	})

	t.Run("required fails observably", func(t *testing.T) {
		observer := &recordingObserver{}
		searcher, err := hybrid.NewSearcher(&fakeSearcher{}, nil, observer, hybrid.Config{Mode: hybrid.SemanticRequired})
		if err != nil {
			t.Fatalf("NewSearcher() error = %v", err)
		}

		hits, err := searcher.Search(context.Background(), query)
		if !errors.Is(err, vector.ErrVectorUnavailable) {
			t.Fatalf("Search() hits = %#v error = %v, want ErrVectorUnavailable", hits, err)
		}
		if strings.Contains(err.Error(), query.Text) {
			t.Fatalf("Search() error exposes query: %v", err)
		}
		events := observer.snapshot()
		if len(events) != 1 || events[0].Backend != "vector" || events[0].Kind != "failure" || events[0].Reason != "vector_unavailable" || events[0].Latency < 0 {
			t.Fatalf("observer events = %#v, want exactly one sanitized failure", events)
		}
	})
}

func TestSearcherPreservesNonNilEmptyFilterSlicesAcrossEveryBackendPath(t *testing.T) {
	newQuery := func() search.Query {
		query := validQuery()
		query.Filter.PathPrefixes = make([]string, 0, 2)
		query.Filter.Languages = make([]source.Language, 0, 2)
		return query
	}
	assertExactEmptyFilter := func(t *testing.T, got, want search.Query) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("backend query = %#v, want exact shape %#v", got, want)
		}
		if got.Filter.PathPrefixes == nil || got.Filter.Languages == nil {
			t.Errorf("backend filter = %#v, want non-nil empty slices", got.Filter)
		}
	}

	t.Run("semantic off", func(t *testing.T) {
		query := newQuery()
		lexical := &fakeSearcher{searchFn: func(_ context.Context, got search.Query) ([]search.Hit, error) {
			assertExactEmptyFilter(t, got, query)
			got.Filter.PathPrefixes = append(got.Filter.PathPrefixes, "PRIVATE_LEXICAL_MUTATION")
			got.Filter.Languages = append(got.Filter.Languages, source.LanguagePython)
			return nil, nil
		}}
		searcher, err := hybrid.NewSearcher(lexical, nil, nil, hybrid.Config{Mode: hybrid.SemanticOff})
		if err != nil {
			t.Fatalf("NewSearcher() error = %v", err)
		}
		if _, err := searcher.Search(context.Background(), query); err != nil {
			t.Fatalf("Search() error = %v", err)
		}
		assertExactEmptyFilter(t, query, newQuery())
	})

	t.Run("preferred without vector", func(t *testing.T) {
		query := newQuery()
		lexical := &fakeSearcher{searchFn: func(_ context.Context, got search.Query) ([]search.Hit, error) {
			assertExactEmptyFilter(t, got, query)
			got.Filter.PathPrefixes = append(got.Filter.PathPrefixes, "PRIVATE_LEXICAL_MUTATION")
			got.Filter.Languages = append(got.Filter.Languages, source.LanguagePython)
			return nil, nil
		}}
		searcher, err := hybrid.NewSearcher(lexical, nil, nil, hybrid.Config{Mode: hybrid.SemanticPreferred})
		if err != nil {
			t.Fatalf("NewSearcher() error = %v", err)
		}
		if _, err := searcher.Search(context.Background(), query); err != nil {
			t.Fatalf("Search() error = %v", err)
		}
		assertExactEmptyFilter(t, query, newQuery())
	})

	t.Run("enabled backends receive independent clones", func(t *testing.T) {
		query := newQuery()
		backendQuery := query
		backendQuery.Limit = 20
		lexicalMutated := make(chan struct{})
		lexical := &fakeSearcher{searchFn: func(_ context.Context, got search.Query) ([]search.Hit, error) {
			assertExactEmptyFilter(t, got, backendQuery)
			got.Filter.PathPrefixes = append(got.Filter.PathPrefixes, "PRIVATE_LEXICAL_MUTATION")
			got.Filter.Languages = append(got.Filter.Languages, source.LanguagePython)
			close(lexicalMutated)
			return nil, nil
		}}
		vectorSearcher := &fakeSearcher{searchFn: func(_ context.Context, got search.Query) ([]search.Hit, error) {
			<-lexicalMutated
			assertExactEmptyFilter(t, got, backendQuery)
			got.Filter.PathPrefixes = append(got.Filter.PathPrefixes, "PRIVATE_VECTOR_MUTATION")
			got.Filter.Languages = append(got.Filter.Languages, source.LanguageTypeScript)
			return nil, nil
		}}
		searcher, err := hybrid.NewSearcher(lexical, vectorSearcher, nil, hybrid.Config{Mode: hybrid.SemanticPreferred})
		if err != nil {
			t.Fatalf("NewSearcher() error = %v", err)
		}
		if _, err := searcher.Search(context.Background(), query); err != nil {
			t.Fatalf("Search() error = %v", err)
		}
		assertExactEmptyFilter(t, query, newQuery())
		if got := query.Filter.PathPrefixes[:1][0]; got != "" {
			t.Fatalf("caller path backing array mutated to %q", got)
		}
		if got := query.Filter.Languages[:1][0]; got != "" {
			t.Fatalf("caller language backing array mutated to %q", got)
		}
	})
}

func TestSearcherIsolatesObserverPanic(t *testing.T) {
	lexicalHit := search.Hit{Chunk: testChunk("lexical", "lexical.py", 1, 2, "LEXICAL_SOURCE"), Score: 0.7}
	searcher, err := hybrid.NewSearcher(
		&fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
			return []search.Hit{lexicalHit}, nil
		}},
		nil,
		observerFunc(func(context.Context, hybrid.Event) {
			panic("PRIVATE_OBSERVER_PANIC_CANARY")
		}),
		hybrid.Config{Mode: hybrid.SemanticPreferred},
	)
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}

	hits, err := searcher.Search(context.Background(), validQuery())
	if err != nil {
		t.Fatalf("Search() error = %v, want observer panic isolated", err)
	}
	if len(hits) != 1 || !reflect.DeepEqual(hits[0], lexicalHit) {
		t.Fatalf("Search() hits = %#v, want lexical fallback %#v", hits, lexicalHit)
	}
}

func TestSearcherParentCancellationDuringObserverWinsOverFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	observerCalled := false
	searcher, err := hybrid.NewSearcher(
		&fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
			return []search.Hit{{Chunk: testChunk("lexical", "lexical.py", 1, 2, "LEXICAL_SOURCE"), Score: 0.7}}, nil
		}},
		nil,
		observerFunc(func(observed context.Context, event hybrid.Event) {
			observerCalled = true
			if observed.Done() == nil || observed.Done() != ctx.Done() {
				t.Errorf("Observe() context is not the cancellable parent context")
			}
			if event.Backend != "vector" || event.Kind != "fallback" || event.Reason != "vector_unavailable" {
				t.Errorf("Observe() event = %#v, want fixed fallback vocabulary", event)
			}
			cancel()
		}),
		hybrid.Config{Mode: hybrid.SemanticPreferred},
	)
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}

	hits, err := searcher.Search(ctx, validQuery())
	if !observerCalled {
		t.Fatal("Observe() was not called")
	}
	if !errors.Is(err, context.Canceled) || len(hits) != 0 {
		t.Fatalf("Search() hits = %#v error = %v, want parent context.Canceled instead of fallback", hits, err)
	}
}

func TestSearcherSerializesConcurrentCallsToNonThreadsafeObserver(t *testing.T) {
	observer := newUnsafeBlockingObserver()
	secondBackendReady := make(chan struct{})
	var backendCalls atomic.Int64
	lexical := &fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
		if backendCalls.Add(1) == 2 {
			close(secondBackendReady)
		}
		return []search.Hit{{Chunk: testChunk("lexical", "lexical.py", 1, 2, "LEXICAL_SOURCE"), Score: 0.7}}, nil
	}}
	searcher, err := hybrid.NewSearcher(lexical, nil, observer, hybrid.Config{Mode: hybrid.SemanticPreferred})
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}
	results := make(chan error, 2)
	go func() {
		_, searchErr := searcher.Search(context.Background(), validQuery())
		results <- searchErr
	}()
	waitForSignal(t, observer.firstEntered, "first observer call")
	go func() {
		_, searchErr := searcher.Search(context.Background(), validQuery())
		results <- searchErr
	}()
	waitForSignal(t, secondBackendReady, "second search fallback")

	select {
	case <-observer.laterEntered:
		// An unserialized observer reaches this branch while the first call is blocked.
	case <-time.After(50 * time.Millisecond):
	}
	close(observer.releaseFirst)
	for range 2 {
		select {
		case searchErr := <-results:
			if searchErr != nil {
				t.Fatalf("Search() error = %v", searchErr)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent Search() did not return")
		}
	}
	if observer.overlap {
		t.Fatal("Observe() calls overlapped on a non-threadsafe observer")
	}
}

func TestSearcherBlockingObserverHonorsParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	observerStarted := make(chan struct{})
	observerReturned := make(chan struct{})
	searcher, err := hybrid.NewSearcher(
		&fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
			return []search.Hit{{Chunk: testChunk("lexical", "lexical.py", 1, 2, "LEXICAL_SOURCE"), Score: 0.7}}, nil
		}},
		nil,
		observerFunc(func(observed context.Context, _ hybrid.Event) {
			close(observerStarted)
			<-observed.Done()
			close(observerReturned)
		}),
		hybrid.Config{Mode: hybrid.SemanticPreferred},
	)
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, searchErr := searcher.Search(ctx, validQuery())
		result <- searchErr
	}()
	waitForSignal(t, observerStarted, "blocking observer start")
	cancel()
	waitForSignal(t, observerReturned, "blocking observer cancellation return")
	select {
	case searchErr := <-result:
		if !errors.Is(searchErr, context.Canceled) {
			t.Fatalf("Search() error = %v, want parent context.Canceled", searchErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Search() did not return promptly after observer honored cancellation")
	}
}

func TestSearcherStartsBothBackendsBeforeWaitingAndParentCancellationWins(t *testing.T) {
	lexicalStarted := make(chan struct{})
	vectorStarted := make(chan struct{})
	lexical := &fakeSearcher{searchFn: func(ctx context.Context, query search.Query) ([]search.Hit, error) {
		if query.Limit != 20 {
			t.Errorf("lexical limit = %d, want expanded minimum 20", query.Limit)
		}
		close(lexicalStarted)
		<-ctx.Done()
		return nil, errors.New("PRIVATE_LEXICAL_CANCEL_CANARY")
	}}
	vectorSearcher := &fakeSearcher{searchFn: func(ctx context.Context, query search.Query) ([]search.Hit, error) {
		if query.Limit != 20 {
			t.Errorf("vector limit = %d, want expanded minimum 20", query.Limit)
		}
		close(vectorStarted)
		<-ctx.Done()
		return nil, errors.New("PRIVATE_VECTOR_CANCEL_CANARY")
	}}
	observer := &recordingObserver{}
	searcher, err := hybrid.NewSearcher(lexical, vectorSearcher, observer, hybrid.Config{Mode: hybrid.SemanticPreferred})
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, searchErr := searcher.Search(ctx, validQuery())
		result <- searchErr
	}()

	waitForSignal(t, lexicalStarted, "lexical start")
	waitForSignal(t, vectorStarted, "vector start")
	cancel()

	select {
	case searchErr := <-result:
		if !errors.Is(searchErr, context.Canceled) {
			t.Fatalf("Search() error = %v, want parent context.Canceled", searchErr)
		}
		for _, canary := range []string{"PRIVATE_LEXICAL_CANCEL_CANARY", "PRIVATE_VECTOR_CANCEL_CANARY"} {
			if strings.Contains(searchErr.Error(), canary) {
				t.Fatalf("Search() error exposes backend cause: %v", searchErr)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("Search() did not return promptly after parent cancellation")
	}
	if len(observer.snapshot()) != 0 {
		t.Fatalf("parent cancellation events = %#v, want none", observer.snapshot())
	}
}

func TestSearcherSemanticTimeoutFallsBackWithoutCancelingLexical(t *testing.T) {
	lexicalStarted := make(chan struct{})
	lexicalRelease := make(chan struct{})
	vectorTimedOut := make(chan struct{})
	lexicalHits := []search.Hit{
		{Chunk: testChunk("first", "first.py", 1, 2, "FIRST_SOURCE"), Score: 0.88},
		{Chunk: testChunk("second", "second.py", 1, 2, "SECOND_SOURCE"), Score: 0.44},
	}
	lexical := &fakeSearcher{searchFn: func(ctx context.Context, _ search.Query) ([]search.Hit, error) {
		close(lexicalStarted)
		select {
		case <-lexicalRelease:
			if err := ctx.Err(); err != nil {
				t.Errorf("semantic timeout canceled lexical context: %v", err)
			}
			return lexicalHits, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	vectorSearcher := &fakeSearcher{searchFn: func(ctx context.Context, _ search.Query) ([]search.Hit, error) {
		<-ctx.Done()
		close(vectorTimedOut)
		return nil, errors.New("PRIVATE_TIMEOUT_CANARY: " + ctx.Err().Error())
	}}
	observer := &recordingObserver{}
	searcher, err := hybrid.NewSearcher(lexical, vectorSearcher, observer, hybrid.Config{
		Mode:            hybrid.SemanticPreferred,
		SemanticTimeout: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}
	query := validQuery()
	query.Limit = 1
	type outcome struct {
		hits []search.Hit
		err  error
	}
	result := make(chan outcome, 1)
	go func() {
		hits, searchErr := searcher.Search(context.Background(), query)
		result <- outcome{hits: hits, err: searchErr}
	}()

	waitForSignal(t, lexicalStarted, "lexical start")
	waitForSignal(t, vectorTimedOut, "semantic timeout")
	select {
	case got := <-result:
		t.Fatalf("Search() returned before mandatory lexical backend: %#v", got)
	case <-time.After(10 * time.Millisecond):
	}
	close(lexicalRelease)

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("Search() error = %v", got.err)
		}
		if len(got.hits) != 1 || !reflect.DeepEqual(got.hits[0], lexicalHits[0]) {
			t.Fatalf("Search() hits = %#v, want original lexical top hit %#v", got.hits, lexicalHits[0])
		}
	case <-time.After(time.Second):
		t.Fatal("Search() did not return after lexical completion")
	}

	events := observer.snapshot()
	if len(events) != 1 || events[0].Backend != "vector" || events[0].Kind != "fallback" || events[0].Reason != "timeout" || events[0].Latency < 0 {
		t.Fatalf("observer events = %#v, want sanitized timeout fallback", events)
	}
	if strings.Contains(events[0].Reason, "PRIVATE_TIMEOUT_CANARY") {
		t.Fatalf("observer event leaks backend error: %#v", events[0])
	}
}

func TestSearcherRequiredSemanticTimeoutReturnsTypedFailure(t *testing.T) {
	vectorSearcher := &fakeSearcher{searchFn: func(ctx context.Context, _ search.Query) ([]search.Hit, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("PRIVATE_REQUIRED_TIMEOUT_CANARY: %w", ctx.Err())
	}}
	observer := &recordingObserver{}
	searcher, err := hybrid.NewSearcher(
		&fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
			return []search.Hit{{Chunk: testChunk("lexical", "lexical.py", 1, 2, "LEXICAL_SOURCE"), Score: 1}}, nil
		}},
		vectorSearcher,
		observer,
		hybrid.Config{Mode: hybrid.SemanticRequired, SemanticTimeout: 20 * time.Millisecond},
	)
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}

	hits, err := searcher.Search(context.Background(), validQuery())
	if !errors.Is(err, context.DeadlineExceeded) || len(hits) != 0 {
		t.Fatalf("Search() hits = %#v error = %v, want context.DeadlineExceeded", hits, err)
	}
	if strings.Contains(err.Error(), "PRIVATE_REQUIRED_TIMEOUT_CANARY") {
		t.Fatalf("Search() error exposes vector cause: %v", err)
	}
	events := observer.snapshot()
	if len(events) != 1 || events[0].Backend != "vector" || events[0].Kind != "failure" || events[0].Reason != "timeout" || events[0].Latency < 0 {
		t.Fatalf("observer events = %#v, want sanitized timeout failure", events)
	}
}

func TestSearcherIgnoresVectorSuccessReturnedAfterSemanticTimeout(t *testing.T) {
	lexicalRelease := make(chan struct{})
	vectorReturned := make(chan struct{})
	lexicalHit := search.Hit{Chunk: testChunk("lexical", "lexical.py", 1, 2, "LEXICAL_SOURCE"), Score: 0.9}
	lexical := &fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
		<-lexicalRelease
		return []search.Hit{lexicalHit}, nil
	}}
	vectorSearcher := &fakeSearcher{searchFn: func(ctx context.Context, _ search.Query) ([]search.Hit, error) {
		<-ctx.Done()
		close(vectorReturned)
		return []search.Hit{{Chunk: testChunk("late-vector", "vector.py", 1, 2, "PRIVATE_LATE_VECTOR_SOURCE"), Score: 1}}, nil
	}}
	observer := &recordingObserver{}
	searcher, err := hybrid.NewSearcher(lexical, vectorSearcher, observer, hybrid.Config{
		Mode:            hybrid.SemanticPreferred,
		SemanticTimeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}
	type outcome struct {
		hits []search.Hit
		err  error
	}
	result := make(chan outcome, 1)
	go func() {
		hits, searchErr := searcher.Search(context.Background(), validQuery())
		result <- outcome{hits: hits, err: searchErr}
	}()
	waitForSignal(t, vectorReturned, "late vector result")
	close(lexicalRelease)

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("Search() error = %v", got.err)
		}
		if len(got.hits) != 1 || !reflect.DeepEqual(got.hits[0], lexicalHit) {
			t.Fatalf("Search() hits = %#v, want lexical fallback without late vector evidence", got.hits)
		}
	case <-time.After(time.Second):
		t.Fatal("Search() did not return")
	}
	events := observer.snapshot()
	if len(events) != 1 || events[0].Kind != "fallback" || events[0].Reason != "timeout" {
		t.Fatalf("observer events = %#v, want timeout fallback", events)
	}
}

func TestSearcherClassifiesOnlyTypedVectorUnavailabilityAsDegradable(t *testing.T) {
	tests := []struct {
		name   string
		cause  error
		reason string
	}{
		{name: "semantic unavailable", cause: embedding.ErrSemanticUnavailable, reason: "semantic_unavailable"},
		{name: "vector unavailable", cause: vector.ErrVectorUnavailable, reason: "vector_unavailable"},
		{name: "incompatible space", cause: vector.ErrIncompatibleSpace, reason: "incompatible_space"},
		{name: "generation changed", cause: vector.ErrGenerationChanged, reason: "generation_changed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, mode := range []hybrid.SemanticMode{hybrid.SemanticPreferred, hybrid.SemanticRequired} {
				t.Run(string(mode), func(t *testing.T) {
					lexicalHits := []search.Hit{
						{Chunk: testChunk("first", "first.py", 1, 2, "FIRST_SOURCE"), Score: 0.9},
						{Chunk: testChunk("second", "second.py", 1, 2, "SECOND_SOURCE"), Score: 0.4},
					}
					lexical := &fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
						return lexicalHits, nil
					}}
					vectorSearcher := &fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
						return nil, fmt.Errorf("PRIVATE_VECTOR_CANARY: %w", test.cause)
					}}
					observer := &recordingObserver{}
					searcher, err := hybrid.NewSearcher(lexical, vectorSearcher, observer, hybrid.Config{Mode: mode})
					if err != nil {
						t.Fatalf("NewSearcher() error = %v", err)
					}
					query := validQuery()
					query.Limit = 1

					hits, searchErr := searcher.Search(context.Background(), query)
					wantKind := "fallback"
					if mode == hybrid.SemanticPreferred {
						if searchErr != nil {
							t.Fatalf("Search() error = %v, want lexical fallback", searchErr)
						}
						if len(hits) != 1 || !reflect.DeepEqual(hits[0], lexicalHits[0]) {
							t.Fatalf("Search() hits = %#v, want lexical top %#v", hits, lexicalHits[0])
						}
					} else {
						wantKind = "failure"
						if !errors.Is(searchErr, test.cause) || len(hits) != 0 {
							t.Fatalf("Search() hits = %#v error = %v, want sanitized %v", hits, searchErr, test.cause)
						}
						if strings.Contains(searchErr.Error(), "PRIVATE_VECTOR_CANARY") {
							t.Fatalf("Search() error exposes vector cause: %v", searchErr)
						}
					}

					events := observer.snapshot()
					if len(events) != 1 || events[0].Backend != "vector" || events[0].Kind != wantKind || events[0].Reason != test.reason || events[0].Latency < 0 {
						t.Fatalf("observer events = %#v, want one %s/%s", events, wantKind, test.reason)
					}
					if strings.Contains(events[0].Reason, "PRIVATE_VECTOR_CANARY") {
						t.Fatalf("observer event exposes vector cause: %#v", events[0])
					}
				})
			}
		})
	}
}

func TestSearcherFusesRanksDeduplicatesAndNormalizesWithoutRawScoreMixing(t *testing.T) {
	chunks := map[string]source.Chunk{
		"a": testChunk("a", "a.py", 1, 2, "SOURCE_A"),
		"b": testChunk("b", "b.py", 1, 2, "SOURCE_B"),
		"c": testChunk("c", "c.py", 1, 2, "SOURCE_C"),
		"d": testChunk("d", "d.py", 1, 2, "SOURCE_D"),
	}
	lexicalHits := []search.Hit{
		{Chunk: chunks["a"], Score: -1000},
		{Chunk: chunks["b"], Score: 0.000001},
		{Chunk: chunks["c"], Score: 999},
	}
	vectorHits := []search.Hit{
		{Chunk: chunks["c"], Score: -999},
		{Chunk: chunks["b"], Score: 1000},
		{Chunk: chunks["d"], Score: 0.5},
	}
	lexical := &fakeSearcher{searchFn: func(_ context.Context, query search.Query) ([]search.Hit, error) {
		if query.Limit != 20 {
			t.Fatalf("lexical candidate limit = %d, want 20", query.Limit)
		}
		return lexicalHits, nil
	}}
	vectorSearcher := &fakeSearcher{searchFn: func(_ context.Context, query search.Query) ([]search.Hit, error) {
		if query.Limit != 20 {
			t.Fatalf("vector candidate limit = %d, want 20", query.Limit)
		}
		return vectorHits, nil
	}}
	observer := &recordingObserver{}
	searcher, err := hybrid.NewSearcher(lexical, vectorSearcher, observer, hybrid.Config{Mode: hybrid.SemanticPreferred})
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}
	query := validQuery()
	query.Limit = 4

	hits, err := searcher.Search(context.Background(), query)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	wantIDs := []string{"c", "b", "a", "d"}
	wantScores := []float64{
		0.9841269841269841,
		0.9838709677419355,
		0.5,
		0.48412698412698413,
	}
	if len(hits) != len(wantIDs) {
		t.Fatalf("Search() hits = %#v, want %d", hits, len(wantIDs))
	}
	for position, hit := range hits {
		if hit.Chunk.ID != wantIDs[position] {
			t.Errorf("hit %d ID = %q, want %q", position, hit.Chunk.ID, wantIDs[position])
		}
		if math.Abs(hit.Score-wantScores[position]) > 1e-12 {
			t.Errorf("hit %d score = %.15f, want %.15f", position, hit.Score, wantScores[position])
		}
		if hit.Score <= 0 || hit.Score > 1 || math.IsNaN(hit.Score) || math.IsInf(hit.Score, 0) {
			t.Errorf("hit %d score = %v, want finite normalized (0,1]", position, hit.Score)
		}
		if !reflect.DeepEqual(hit.Chunk, chunks[hit.Chunk.ID]) {
			t.Errorf("hit %d chunk = %#v, want canonical %#v", position, hit.Chunk, chunks[hit.Chunk.ID])
		}
	}
	if len(observer.snapshot()) != 0 {
		t.Fatalf("successful hybrid events = %#v, want none", observer.snapshot())
	}
}

func TestSearcherValidatesCompleteLexicalFallbackBeforeApplyingLimit(t *testing.T) {
	valid := search.Hit{Chunk: testChunk("valid", "valid.py", 1, 2, "VALID_SOURCE"), Score: 0.8}
	invalidTail := search.Hit{Chunk: testChunk("tail", "tail.py", 1, 2, "PRIVATE_TAIL_SOURCE_CANARY"), Score: math.NaN()}
	lexical := &fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
		return []search.Hit{valid, invalidTail}, nil
	}}
	observer := &recordingObserver{}
	searcher, err := hybrid.NewSearcher(lexical, nil, observer, hybrid.Config{Mode: hybrid.SemanticPreferred})
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}
	query := validQuery()
	query.Limit = 1

	hits, err := searcher.Search(context.Background(), query)
	if !errors.Is(err, hybrid.ErrEvidenceIntegrity) {
		t.Fatalf("Search() hits = %#v error = %v, want ErrEvidenceIntegrity from invalid tail", hits, err)
	}
	if strings.Contains(err.Error(), "PRIVATE_TAIL_SOURCE_CANARY") {
		t.Fatalf("Search() error exposes source: %v", err)
	}
	if len(observer.snapshot()) != 0 {
		t.Fatalf("unsafe fallback events = %#v, want none", observer.snapshot())
	}
}

func TestSearcherTreatsVectorIntegrityAndUnknownErrorsAsFatal(t *testing.T) {
	tests := []struct {
		name      string
		cause     error
		want      error
		wantCause bool
	}{
		{name: "vector integrity", cause: vector.ErrVectorIntegrity, want: hybrid.ErrEvidenceIntegrity, wantCause: true},
		{name: "invalid query vector", cause: vector.ErrInvalidQueryVector, want: hybrid.ErrVectorFailure, wantCause: true},
		{name: "sanitized vector backend", cause: vector.ErrBackend, want: hybrid.ErrVectorFailure, wantCause: true},
		{name: "unknown", cause: errors.New("PRIVATE_UNKNOWN_VECTOR_CANARY"), want: hybrid.ErrVectorFailure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lexical := &fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
				return []search.Hit{{Chunk: testChunk("lexical", "lexical.py", 1, 2, "LEXICAL_SOURCE"), Score: 0.8}}, nil
			}}
			vectorSearcher := &fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
				return nil, fmt.Errorf("PRIVATE_VECTOR_FAILURE_CANARY: %w", test.cause)
			}}
			searcher, err := hybrid.NewSearcher(lexical, vectorSearcher, &recordingObserver{}, hybrid.Config{Mode: hybrid.SemanticPreferred})
			if err != nil {
				t.Fatalf("NewSearcher() error = %v", err)
			}

			hits, searchErr := searcher.Search(context.Background(), validQuery())
			if !errors.Is(searchErr, test.want) || len(hits) != 0 {
				t.Fatalf("Search() hits = %#v error = %v, want fatal %v", hits, searchErr, test.want)
			}
			if test.wantCause && !errors.Is(searchErr, test.cause) {
				t.Fatalf("Search() error = %v, want typed cause %v", searchErr, test.cause)
			}
			for _, canary := range []string{"PRIVATE_VECTOR_FAILURE_CANARY", "PRIVATE_UNKNOWN_VECTOR_CANARY"} {
				if strings.Contains(searchErr.Error(), canary) {
					t.Fatalf("Search() error exposes vector cause: %v", searchErr)
				}
			}
		})
	}
}

func TestSearcherRejectsMalformedBranchEvidenceBeforeLimit(t *testing.T) {
	valid := search.Hit{Chunk: testChunk("valid", "valid.py", 1, 2, "VALID_SOURCE"), Score: 0.8}
	tests := []struct {
		name   string
		mutate func(*search.Hit)
	}{
		{name: "duplicate ID", mutate: func(hit *search.Hit) { hit.Chunk.ID = valid.Chunk.ID }},
		{name: "empty ID", mutate: func(hit *search.Hit) { hit.Chunk.ID = "" }},
		{name: "empty text", mutate: func(hit *search.Hit) { hit.Chunk.Text = "" }},
		{name: "invalid reference", mutate: func(hit *search.Hit) { hit.Chunk.Reference.Path = "private/../PRIVATE_PATH_CANARY" }},
		{name: "NaN score", mutate: func(hit *search.Hit) { hit.Score = math.NaN() }},
		{name: "positive infinity", mutate: func(hit *search.Hit) { hit.Score = math.Inf(1) }},
		{name: "negative infinity", mutate: func(hit *search.Hit) { hit.Score = math.Inf(-1) }},
	}
	for _, test := range tests {
		for _, branch := range []string{"lexical", "vector"} {
			t.Run(test.name+"/"+branch, func(t *testing.T) {
				bad := search.Hit{Chunk: testChunk("bad", "bad.py", 3, 4, "PRIVATE_BAD_SOURCE_CANARY"), Score: 0.2}
				test.mutate(&bad)
				lexicalHits := []search.Hit{valid}
				vectorHits := []search.Hit{}
				if branch == "lexical" {
					lexicalHits = append(lexicalHits, bad)
				} else {
					vectorHits = []search.Hit{
						{Chunk: testChunk("vector-valid", "vector.py", 1, 2, "VECTOR_VALID"), Score: 0.7},
						bad,
					}
				}
				searcher, err := hybrid.NewSearcher(
					&fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) { return lexicalHits, nil }},
					&fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) { return vectorHits, nil }},
					nil,
					hybrid.Config{Mode: hybrid.SemanticPreferred},
				)
				if err != nil {
					t.Fatalf("NewSearcher() error = %v", err)
				}
				query := validQuery()
				query.Limit = 1

				hits, searchErr := searcher.Search(context.Background(), query)
				if !errors.Is(searchErr, hybrid.ErrEvidenceIntegrity) {
					t.Fatalf("Search() hits = %#v error = %v, want ErrEvidenceIntegrity", hits, searchErr)
				}
				for _, canary := range []string{"PRIVATE_BAD_SOURCE_CANARY", "PRIVATE_PATH_CANARY"} {
					if strings.Contains(searchErr.Error(), canary) {
						t.Fatalf("Search() error exposes evidence: %v", searchErr)
					}
				}
			})
		}
	}
}

func TestSearcherRejectsConflictingCanonicalChunks(t *testing.T) {
	canonical := testChunk("shared", "shared.py", 3, 5, "SHARED_SOURCE")
	tests := []struct {
		name   string
		mutate func(*source.Chunk)
	}{
		{name: "text", mutate: func(chunk *source.Chunk) { chunk.Text = "PRIVATE_CONFLICT_TEXT_CANARY" }},
		{name: "language", mutate: func(chunk *source.Chunk) { chunk.Language = source.LanguageTypeScript }},
		{name: "symbol", mutate: func(chunk *source.Chunk) { chunk.SymbolName = "PRIVATE_CONFLICT_SYMBOL_CANARY" }},
		{name: "path", mutate: func(chunk *source.Chunk) { chunk.Reference.Path = "other.py" }},
		{name: "start line", mutate: func(chunk *source.Chunk) { chunk.Reference.StartLine = 2 }},
		{name: "end line", mutate: func(chunk *source.Chunk) { chunk.Reference.EndLine = 6 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conflicting := canonical
			test.mutate(&conflicting)
			searcher, err := hybrid.NewSearcher(
				&fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
					return []search.Hit{{Chunk: canonical, Score: 0.8}}, nil
				}},
				&fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
					return []search.Hit{{Chunk: conflicting, Score: 0.9}}, nil
				}},
				nil,
				hybrid.Config{Mode: hybrid.SemanticPreferred},
			)
			if err != nil {
				t.Fatalf("NewSearcher() error = %v", err)
			}

			hits, searchErr := searcher.Search(context.Background(), validQuery())
			if !errors.Is(searchErr, hybrid.ErrEvidenceIntegrity) {
				t.Fatalf("Search() hits = %#v error = %v, want ErrEvidenceIntegrity", hits, searchErr)
			}
			for _, canary := range []string{"PRIVATE_CONFLICT_TEXT_CANARY", "PRIVATE_CONFLICT_SYMBOL_CANARY"} {
				if strings.Contains(searchErr.Error(), canary) {
					t.Fatalf("Search() error exposes conflicting evidence: %v", searchErr)
				}
			}
		})
	}
}

func TestSearcherUsesEveryDeterministicTieBreaker(t *testing.T) {
	t.Run("contributors", func(t *testing.T) {
		a := testChunk("a", "z-a.py", 1, 1, "A")
		b := testChunk("b", "z-b.py", 1, 1, "B")
		lexicalHits := []search.Hit{
			{Chunk: b, Score: 1},
			{Chunk: testChunk("lexical-filler", "lf.py", 1, 1, "LF"), Score: 1},
			{Chunk: a, Score: 1},
		}
		vectorHits := []search.Hit{
			{Chunk: testChunk("vector-filler-1", "vf1.py", 1, 1, "VF1"), Score: 1},
			{Chunk: testChunk("vector-filler-2", "vf2.py", 1, 1, "VF2"), Score: 1},
			{Chunk: a, Score: 1},
		}
		hits := searchWithFixedHits(t, lexicalHits, vectorHits, hybrid.Config{Mode: hybrid.SemanticPreferred, RRFK: 1}, 10)
		assertHitBefore(t, hits, "a", "b", "two-backend contributor tie-break")
	})

	t.Run("best rank", func(t *testing.T) {
		a := testChunk("a", "z-a.py", 1, 1, "A")
		b := testChunk("b", "z-b.py", 1, 1, "B")
		lexicalHits := []search.Hit{
			{Chunk: a, Score: 1},
			{Chunk: testChunk("lexical-filler", "lf.py", 1, 1, "LF"), Score: 1},
			{Chunk: b, Score: 1},
		}
		vectorHits := []search.Hit{
			{Chunk: testChunk("vector-filler-1", "vf1.py", 1, 1, "VF1"), Score: 1},
			{Chunk: testChunk("vector-filler-2", "vf2.py", 1, 1, "VF2"), Score: 1},
			{Chunk: b, Score: 1},
			{Chunk: testChunk("vector-filler-4", "vf4.py", 1, 1, "VF4"), Score: 1},
			{Chunk: testChunk("vector-filler-5", "vf5.py", 1, 1, "VF5"), Score: 1},
			{Chunk: testChunk("vector-filler-6", "vf6.py", 1, 1, "VF6"), Score: 1},
			{Chunk: a, Score: 1},
		}
		hits := searchWithFixedHits(t, lexicalHits, vectorHits, hybrid.Config{
			Mode:          hybrid.SemanticPreferred,
			RRFK:          1,
			LexicalWeight: 1,
			VectorWeight:  2,
		}, 20)
		assertHitBefore(t, hits, "a", "b", "best-rank tie-break")
	})

	tests := []struct {
		name string
		a    source.Chunk
		b    source.Chunk
	}{
		{name: "path", a: testChunk("a", "a.py", 1, 1, "A"), b: testChunk("b", "b.py", 1, 1, "B")},
		{name: "start line", a: testChunk("a", "same.py", 1, 1, "A"), b: testChunk("b", "same.py", 2, 2, "B")},
		{name: "end line", a: testChunk("a", "same.py", 1, 1, "A"), b: testChunk("b", "same.py", 1, 2, "B")},
		{name: "chunk ID", a: testChunk("a", "same.py", 1, 1, "A"), b: testChunk("b", "same.py", 1, 1, "B")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lexicalHits := []search.Hit{{Chunk: test.b, Score: 1000}, {Chunk: test.a, Score: -1000}}
			vectorHits := []search.Hit{{Chunk: test.a, Score: 0}, {Chunk: test.b, Score: 0}}
			var want []string
			for run := 0; run < 25; run++ {
				hits := searchWithFixedHits(t, lexicalHits, vectorHits, hybrid.Config{Mode: hybrid.SemanticPreferred}, 2)
				got := hitIDs(hits)
				if run == 0 {
					want = got
				}
				if !reflect.DeepEqual(got, []string{"a", "b"}) || !reflect.DeepEqual(got, want) {
					t.Fatalf("run %d IDs = %v, want deterministic [a b]", run, got)
				}
			}
		})
	}
}

func TestSearcherExpandsAndClampsCandidateWindowWithoutOverflow(t *testing.T) {
	tests := []struct {
		name       string
		limit      int
		multiplier int
		want       int
	}{
		{name: "minimum", limit: 1, multiplier: 4, want: 20},
		{name: "product", limit: 6, multiplier: 4, want: 24},
		{name: "custom multiplier", limit: 30, multiplier: 1, want: 30},
		{name: "maximum", limit: 100, multiplier: 4, want: 200},
		{name: "overflow safe", limit: 200, multiplier: math.MaxInt, want: 200},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lexicalLimit := 0
			vectorLimit := 0
			lexical := &fakeSearcher{searchFn: func(_ context.Context, query search.Query) ([]search.Hit, error) {
				lexicalLimit = query.Limit
				return nil, nil
			}}
			vectorSearcher := &fakeSearcher{searchFn: func(_ context.Context, query search.Query) ([]search.Hit, error) {
				vectorLimit = query.Limit
				return nil, nil
			}}
			searcher, err := hybrid.NewSearcher(lexical, vectorSearcher, nil, hybrid.Config{
				Mode:                hybrid.SemanticPreferred,
				CandidateMultiplier: test.multiplier,
			})
			if err != nil {
				t.Fatalf("NewSearcher() error = %v", err)
			}
			query := validQuery()
			query.Limit = test.limit
			wantQuery := cloneQuery(query)

			hits, err := searcher.Search(context.Background(), query)
			if err != nil {
				t.Fatalf("Search() error = %v", err)
			}
			if len(hits) != 0 || lexicalLimit != test.want || vectorLimit != test.want {
				t.Fatalf("Search() hits = %#v limits = lexical:%d vector:%d, want empty and %d/%d", hits, lexicalLimit, vectorLimit, test.want, test.want)
			}
			if !reflect.DeepEqual(query, wantQuery) {
				t.Fatalf("Search() mutated caller query = %#v, want %#v", query, wantQuery)
			}
		})
	}
}

func TestSearcherIsolatesConcurrentFiltersAndDoesNotMutateBackendHits(t *testing.T) {
	query := validQuery()
	query.Filter = search.Filter{
		PathPrefixes: []string{"internal", "pkg"},
		Languages:    []source.Language{source.LanguagePython, source.LanguageTypeScript},
	}
	wantQuery := cloneQuery(query)
	lexicalHits := []search.Hit{{Chunk: testChunk("lexical", "internal/lexical.py", 1, 2, "LEXICAL_SOURCE"), Score: 0.8}}
	vectorHits := []search.Hit{{Chunk: testChunk("vector", "pkg/vector.ts", 1, 2, "VECTOR_SOURCE"), Score: 0.7}}
	wantLexicalHits := append([]search.Hit(nil), lexicalHits...)
	wantVectorHits := append([]search.Hit(nil), vectorHits...)
	lexicalMutated := make(chan struct{})
	lexical := &fakeSearcher{searchFn: func(_ context.Context, backendQuery search.Query) ([]search.Hit, error) {
		backendQuery.Filter.PathPrefixes[0] = "PRIVATE_LEXICAL_MUTATION"
		backendQuery.Filter.Languages[0] = source.LanguageTypeScript
		close(lexicalMutated)
		return lexicalHits, nil
	}}
	vectorSearcher := &fakeSearcher{searchFn: func(_ context.Context, backendQuery search.Query) ([]search.Hit, error) {
		<-lexicalMutated
		if !reflect.DeepEqual(backendQuery.Filter, wantQuery.Filter) {
			t.Errorf("vector filter = %#v after lexical mutation, want isolated %#v", backendQuery.Filter, wantQuery.Filter)
		}
		backendQuery.Filter.PathPrefixes[0] = "PRIVATE_VECTOR_MUTATION"
		backendQuery.Filter.Languages[0] = source.LanguageTypeScript
		return vectorHits, nil
	}}
	searcher, err := hybrid.NewSearcher(lexical, vectorSearcher, nil, hybrid.Config{Mode: hybrid.SemanticPreferred})
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}

	if _, err := searcher.Search(context.Background(), query); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if !reflect.DeepEqual(query, wantQuery) {
		t.Fatalf("Search() mutated caller query = %#v, want %#v", query, wantQuery)
	}
	if !reflect.DeepEqual(lexicalHits, wantLexicalHits) || !reflect.DeepEqual(vectorHits, wantVectorHits) {
		t.Fatalf("Search() mutated backend slices = lexical:%#v vector:%#v", lexicalHits, vectorHits)
	}
}

func TestSearcherSanitizesMandatoryLexicalFailuresAndNeverReturnsVectorOnlyEvidence(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		want   error
		reject error
	}{
		{name: "unknown", err: errors.New("PRIVATE_LEXICAL_FAILURE_CANARY"), want: hybrid.ErrLexicalFailure},
		{name: "filter category", err: fmt.Errorf("PRIVATE_LEXICAL_FAILURE_CANARY: %w", search.ErrInvalidFilter), want: search.ErrInvalidFilter},
		{name: "synthetic canceled category", err: fmt.Errorf("PRIVATE_LEXICAL_FAILURE_CANARY: %w", context.Canceled), want: hybrid.ErrLexicalFailure, reject: context.Canceled},
		{name: "synthetic internal deadline", err: fmt.Errorf("PRIVATE_INTERNAL_TIMEOUT_CANARY: %w", context.DeadlineExceeded), want: hybrid.ErrLexicalFailure, reject: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lexical := &fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
				return nil, test.err
			}}
			vectorSearcher := &fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
				return []search.Hit{{Chunk: testChunk("vector-only", "vector.py", 1, 2, "PRIVATE_VECTOR_ONLY_SOURCE"), Score: 1}}, nil
			}}
			observer := &recordingObserver{}
			searcher, err := hybrid.NewSearcher(lexical, vectorSearcher, observer, hybrid.Config{Mode: hybrid.SemanticPreferred})
			if err != nil {
				t.Fatalf("NewSearcher() error = %v", err)
			}

			hits, searchErr := searcher.Search(context.Background(), validQuery())
			if !errors.Is(searchErr, test.want) || len(hits) != 0 {
				t.Fatalf("Search() hits = %#v error = %v, want no vector-only evidence and %v", hits, searchErr, test.want)
			}
			if test.reject != nil && errors.Is(searchErr, test.reject) {
				t.Fatalf("Search() error = %v, must not trust backend-only category %v", searchErr, test.reject)
			}
			for _, canary := range []string{"PRIVATE_LEXICAL_FAILURE_CANARY", "PRIVATE_INTERNAL_TIMEOUT_CANARY", "PRIVATE_VECTOR_ONLY_SOURCE"} {
				if strings.Contains(searchErr.Error(), canary) {
					t.Fatalf("Search() error exposes backend data: %v", searchErr)
				}
			}
			if len(observer.snapshot()) != 0 {
				t.Fatalf("lexical failure events = %#v, want none", observer.snapshot())
			}
		})
	}

	t.Run("actual parent cancellation wins", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		lexical := &fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
			cancel()
			return nil, errors.New("PRIVATE_PARENT_CANCEL_CANARY")
		}}
		vectorSearcher := &fakeSearcher{searchFn: func(ctx context.Context, _ search.Query) ([]search.Hit, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		searcher, err := hybrid.NewSearcher(lexical, vectorSearcher, nil, hybrid.Config{Mode: hybrid.SemanticPreferred})
		if err != nil {
			t.Fatalf("NewSearcher() error = %v", err)
		}

		hits, searchErr := searcher.Search(ctx, validQuery())
		if !errors.Is(searchErr, context.Canceled) || len(hits) != 0 {
			t.Fatalf("Search() hits = %#v error = %v, want actual parent context.Canceled", hits, searchErr)
		}
		if strings.Contains(searchErr.Error(), "PRIVATE_PARENT_CANCEL_CANARY") {
			t.Fatalf("Search() error exposes lexical cause: %v", searchErr)
		}
	})
}

func TestSearcherCancellationInterruptsLargeFusionAndSort(t *testing.T) {
	const branchSize = 4096
	lexicalHits := make([]search.Hit, branchSize)
	vectorHits := make([]search.Hit, branchSize)
	for position := 0; position < branchSize; position++ {
		lexicalHits[position] = search.Hit{
			Chunk: testChunk(fmt.Sprintf("lexical-%05d", position), fmt.Sprintf("lexical/%05d.py", position), 1, 1, "LEXICAL_SOURCE"),
			Score: 1,
		}
		vectorHits[position] = search.Hit{
			Chunk: testChunk(fmt.Sprintf("vector-%05d", position), fmt.Sprintf("vector/%05d.py", position), 1, 1, "VECTOR_SOURCE"),
			Score: 1,
		}
	}
	searcher, err := hybrid.NewSearcher(
		&fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) { return lexicalHits, nil }},
		&fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) { return vectorHits, nil }},
		nil,
		hybrid.Config{Mode: hybrid.SemanticPreferred},
	)
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}
	ctx := newCancelAfterErrContext(110)

	hits, err := searcher.Search(ctx, validQuery())
	if !errors.Is(err, context.Canceled) || len(hits) != 0 {
		t.Fatalf("Search() hits = %d error = %v, want interruptible cancellation", len(hits), err)
	}
}

func TestSearcherOffPreservesExactLexicalBehaviorAndStaysSilent(t *testing.T) {
	query := validQuery()
	query.Filter = search.Filter{
		PathPrefixes: []string{"internal", "pkg"},
		Languages:    []source.Language{source.LanguagePython, source.LanguageTypeScript},
	}
	wantQuery := cloneQuery(query)
	lexicalHits := []search.Hit{
		{Chunk: testChunk("first", "internal/first.py", 1, 2, "FIRST_SOURCE"), Score: 0.91},
		{Chunk: testChunk("second", "pkg/second.ts", 4, 6, "SECOND_SOURCE"), Score: 0.17},
	}
	lexical := &fakeSearcher{searchFn: func(_ context.Context, got search.Query) ([]search.Hit, error) {
		if !reflect.DeepEqual(got, wantQuery) {
			t.Fatalf("lexical query = %#v, want exact %#v", got, wantQuery)
		}
		got.Filter.PathPrefixes[0] = "PRIVATE_MUTATED_PATH"
		got.Filter.Languages[0] = source.LanguageTypeScript
		return lexicalHits, nil
	}}
	vectorSearcher := &fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) {
		return nil, errors.New("PRIVATE_VECTOR_CANARY")
	}}
	observer := &recordingObserver{}
	searcher, err := hybrid.NewSearcher(lexical, vectorSearcher, observer, hybrid.Config{})
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}

	hits, err := searcher.Search(context.Background(), query)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if !reflect.DeepEqual(hits, lexicalHits) {
		t.Fatalf("Search() hits = %#v, want lexical hits unchanged %#v", hits, lexicalHits)
	}
	if !reflect.DeepEqual(query, wantQuery) {
		t.Fatalf("Search() mutated caller query = %#v, want %#v", query, wantQuery)
	}
	if lexical.callCount() != 1 || vectorSearcher.callCount() != 0 || len(observer.snapshot()) != 0 {
		t.Fatalf("off activity = lexical:%d vector:%d events:%d, want 1,0,0", lexical.callCount(), vectorSearcher.callCount(), len(observer.snapshot()))
	}
}

func TestSearcherRejectsInvalidQueryBeforeBackendsAndObserver(t *testing.T) {
	lexical := &fakeSearcher{}
	vectorSearcher := &fakeSearcher{}
	observer := &recordingObserver{}
	searcher, err := hybrid.NewSearcher(lexical, vectorSearcher, observer, hybrid.Config{Mode: hybrid.SemanticPreferred})
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}

	tests := []struct {
		name  string
		ctx   context.Context
		query search.Query
		want  error
	}{
		{name: "nil context", query: validQuery(), want: hybrid.ErrInvalidQuery},
		{name: "empty repository", ctx: context.Background(), query: search.Query{Text: "PRIVATE_QUERY_CANARY", Limit: 1}, want: hybrid.ErrInvalidQuery},
		{name: "blank repository", ctx: context.Background(), query: search.Query{RepositoryID: "  ", Text: "PRIVATE_QUERY_CANARY", Limit: 1}, want: hybrid.ErrInvalidQuery},
		{name: "blank text", ctx: context.Background(), query: search.Query{RepositoryID: "repository", Text: "  ", Limit: 1}, want: hybrid.ErrInvalidQuery},
		{name: "zero limit", ctx: context.Background(), query: search.Query{RepositoryID: "repository", Text: "PRIVATE_QUERY_CANARY"}, want: hybrid.ErrInvalidQuery},
		{name: "negative limit", ctx: context.Background(), query: search.Query{RepositoryID: "repository", Text: "PRIVATE_QUERY_CANARY", Limit: -1}, want: hybrid.ErrInvalidQuery},
		{name: "limit above maximum", ctx: context.Background(), query: search.Query{RepositoryID: "repository", Text: "PRIVATE_QUERY_CANARY", Limit: 201}, want: hybrid.ErrInvalidQuery},
		{
			name: "invalid filter",
			ctx:  context.Background(),
			query: search.Query{
				RepositoryID: "repository",
				Text:         "PRIVATE_QUERY_CANARY",
				Limit:        1,
				Filter:       search.Filter{Languages: []source.Language{source.LanguageUnknown}},
			},
			want: search.ErrInvalidFilter,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, searchErr := searcher.Search(test.ctx, test.query)
			if !errors.Is(searchErr, hybrid.ErrInvalidQuery) || !errors.Is(searchErr, test.want) {
				t.Fatalf("Search() error = %v, want ErrInvalidQuery and %v", searchErr, test.want)
			}
			if strings.Contains(searchErr.Error(), "PRIVATE_QUERY_CANARY") {
				t.Fatalf("Search() error exposes query text: %v", searchErr)
			}
		})
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = searcher.Search(canceled, validQuery())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Search(canceled) error = %v, want context.Canceled", err)
	}

	if lexical.callCount() != 0 || vectorSearcher.callCount() != 0 || len(observer.snapshot()) != 0 {
		t.Fatalf("invalid query activity = lexical:%d vector:%d events:%d, want zero", lexical.callCount(), vectorSearcher.callCount(), len(observer.snapshot()))
	}
}

type fakeSearcher struct {
	mu       sync.Mutex
	calls    int
	queries  []search.Query
	searchFn func(context.Context, search.Query) ([]search.Hit, error)
}

func (f *fakeSearcher) Search(ctx context.Context, query search.Query) ([]search.Hit, error) {
	f.mu.Lock()
	f.calls++
	f.queries = append(f.queries, cloneQuery(query))
	f.mu.Unlock()
	if f.searchFn == nil {
		return nil, nil
	}
	return f.searchFn(ctx, query)
}

func (f *fakeSearcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type recordingObserver struct {
	mu     sync.Mutex
	events []hybrid.Event
}

type observerFunc func(context.Context, hybrid.Event)

func (f observerFunc) Observe(ctx context.Context, event hybrid.Event) {
	f(ctx, event)
}

type unsafeBlockingObserver struct {
	active       bool
	overlap      bool
	calls        int
	firstEntered chan struct{}
	laterEntered chan struct{}
	releaseFirst chan struct{}
}

func newUnsafeBlockingObserver() *unsafeBlockingObserver {
	return &unsafeBlockingObserver{
		firstEntered: make(chan struct{}),
		laterEntered: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
}

func (o *unsafeBlockingObserver) Observe(context.Context, hybrid.Event) {
	if o.active {
		o.overlap = true
	}
	o.active = true
	o.calls++
	if o.calls == 1 {
		close(o.firstEntered)
		<-o.releaseFirst
	} else {
		close(o.laterEntered)
	}
	o.active = false
}

func (o *recordingObserver) Observe(_ context.Context, event hybrid.Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, event)
}

func (o *recordingObserver) snapshot() []hybrid.Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]hybrid.Event(nil), o.events...)
}

func validQuery() search.Query {
	return search.Query{RepositoryID: "repository", Text: "PRIVATE_QUERY_CANARY", Limit: 3}
}

func cloneQuery(query search.Query) search.Query {
	if query.Filter.PathPrefixes != nil {
		query.Filter.PathPrefixes = append([]string{}, query.Filter.PathPrefixes...)
	}
	if query.Filter.Languages != nil {
		query.Filter.Languages = append([]source.Language{}, query.Filter.Languages...)
	}
	return query
}

func testChunk(id, path string, startLine, endLine int, text string) source.Chunk {
	return source.Chunk{
		ID:         id,
		Text:       text,
		Language:   source.LanguagePython,
		SymbolName: id + "Symbol",
		Reference:  source.Reference{Path: path, StartLine: startLine, EndLine: endLine},
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func searchWithFixedHits(t *testing.T, lexicalHits, vectorHits []search.Hit, config hybrid.Config, limit int) []search.Hit {
	t.Helper()
	searcher, err := hybrid.NewSearcher(
		&fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) { return lexicalHits, nil }},
		&fakeSearcher{searchFn: func(context.Context, search.Query) ([]search.Hit, error) { return vectorHits, nil }},
		nil,
		config,
	)
	if err != nil {
		t.Fatalf("NewSearcher() error = %v", err)
	}
	query := validQuery()
	query.Limit = limit
	hits, err := searcher.Search(context.Background(), query)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	return hits
}

func assertHitBefore(t *testing.T, hits []search.Hit, firstID, secondID, description string) {
	t.Helper()
	firstPosition := -1
	secondPosition := -1
	for position, hit := range hits {
		switch hit.Chunk.ID {
		case firstID:
			firstPosition = position
		case secondID:
			secondPosition = position
		}
	}
	if firstPosition < 0 || secondPosition < 0 || firstPosition >= secondPosition {
		t.Fatalf("%s positions = %q:%d %q:%d in %v", description, firstID, firstPosition, secondID, secondPosition, hitIDs(hits))
	}
}

func hitIDs(hits []search.Hit) []string {
	ids := make([]string, len(hits))
	for position, hit := range hits {
		ids[position] = hit.Chunk.ID
	}
	return ids
}

type cancelAfterErrContext struct {
	context.Context
	remaining atomic.Int64
}

func newCancelAfterErrContext(checks int64) *cancelAfterErrContext {
	ctx := &cancelAfterErrContext{Context: context.Background()}
	ctx.remaining.Store(checks)
	return ctx
}

func (c *cancelAfterErrContext) Err() error {
	if c.remaining.Add(-1) <= 0 {
		return context.Canceled
	}
	return nil
}
