package index_test

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
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestBuilderOffPublishesLexicalGeneration(t *testing.T) {
	tests := []struct {
		name      string
		active    string
		activeErr error
	}{
		{name: "empty repository", activeErr: index.ErrNotFound},
		{name: "existing repository", active: "previous-generation"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			corpus := mustBuilderCorpus(t, "scanner-v4", []source.Chunk{
				builderChunk("first", "first.py", "FIRST = 1"),
				builderChunk("second", "second.py", "SECOND = 2"),
			})
			store := &builderStore{active: tt.active, activeErr: tt.activeErr}
			builder, err := index.NewBuilder(store, nil, index.BuilderConfig{Mode: index.SemanticOff})
			if err != nil {
				t.Fatalf("NewBuilder() error = %v", err)
			}

			report, err := builder.Replace(context.Background(), "repository", corpus)
			if err != nil {
				t.Fatalf("Replace() error = %v", err)
			}

			generation, replaceCalls := store.publishedGeneration()
			if replaceCalls != 1 {
				t.Fatalf("Store.Replace() calls = %d, want 1", replaceCalls)
			}
			if generation.RepositoryID != "repository" || generation.ID == "" || generation.BaseGeneration != tt.active {
				t.Fatalf("published identifiers = repository %q generation %q base %q", generation.RepositoryID, generation.ID, generation.BaseGeneration)
			}
			if generation.CorpusRevision != corpus.Revision || generation.ScanPolicyVersion != corpus.PolicyVersion {
				t.Fatalf("published corpus metadata = revision %q policy %q, want %q %q", generation.CorpusRevision, generation.ScanPolicyVersion, corpus.Revision, corpus.PolicyVersion)
			}
			if !reflect.DeepEqual(generation.Chunks, corpus.Chunks) {
				t.Fatalf("published chunks = %#v, want canonical %#v", generation.Chunks, corpus.Chunks)
			}
			if generation.Profile != nil || generation.Dimensions != 0 || len(generation.Vectors) != 0 || generation.Metric != index.VectorMetricCosine {
				t.Fatalf("published lexical vector metadata = profile %#v dimensions %d vectors %#v metric %q", generation.Profile, generation.Dimensions, generation.Vectors, generation.Metric)
			}
			wantReport := index.Report{
				GenerationID:   generation.ID,
				CorpusRevision: corpus.Revision,
				Chunks:         len(corpus.Chunks),
				Semantic:       "off",
			}
			if !reflect.DeepEqual(report, wantReport) {
				t.Fatalf("Replace() report = %#v, want %#v", report, wantReport)
			}
			if got := store.callSequence(); !reflect.DeepEqual(got, []string{"active", "replace"}) {
				t.Fatalf("store calls = %#v, want active before replace", got)
			}
		})
	}
}

func TestBuilderOffDoesNotCallConfiguredEmbedder(t *testing.T) {
	corpus := mustBuilderCorpus(t, "scanner-v4", []source.Chunk{
		builderChunk("chunk", "chunk.py", "text"),
	})
	embedder := &builderEmbedder{
		profile: embedding.Profile{Fingerprint: "fingerprint", Model: "model"},
		err:     errors.New("off mode must not observe this provider failure"),
	}
	store := &builderStore{activeErr: index.ErrNotFound}
	builder, err := index.NewBuilder(store, embedder, index.BuilderConfig{})
	if err != nil {
		t.Fatalf("NewBuilder() error = %v", err)
	}

	report, err := builder.Replace(context.Background(), "repository", corpus)
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if calls, _, _ := embedder.calls(); calls != 0 {
		t.Fatalf("Embed() calls = %d, want 0 in default off mode", calls)
	}
	if report.Semantic != "off" || store.replaceCallCount() != 1 {
		t.Fatalf("Replace() = %#v with %d publications", report, store.replaceCallCount())
	}
}

func TestNewBuilderValidatesConfiguration(t *testing.T) {
	store := &builderStore{activeErr: index.ErrNotFound}
	embedder := &builderEmbedder{profile: embedding.Profile{Fingerprint: "profile", Model: "model"}}
	tests := []struct {
		name     string
		store    index.Store
		embedder embedding.Embedder
		config   index.BuilderConfig
		wantErr  bool
	}{
		{name: "zero config defaults to off", store: store},
		{name: "explicit off permits nil embedder", store: store, config: index.BuilderConfig{Mode: index.SemanticOff}},
		{name: "preferred accepts embedder", store: store, embedder: embedder, config: index.BuilderConfig{Mode: index.SemanticPreferred}},
		{name: "required accepts embedder", store: store, embedder: embedder, config: index.BuilderConfig{Mode: index.SemanticRequired}},
		{name: "nil store", config: index.BuilderConfig{Mode: index.SemanticOff}, wantErr: true},
		{name: "preferred without embedder", store: store, config: index.BuilderConfig{Mode: index.SemanticPreferred}, wantErr: true},
		{name: "required without embedder", store: store, config: index.BuilderConfig{Mode: index.SemanticRequired}, wantErr: true},
		{name: "unknown mode", store: store, embedder: embedder, config: index.BuilderConfig{Mode: "automatic"}, wantErr: true},
		{name: "negative max chunks", store: store, config: index.BuilderConfig{Mode: index.SemanticOff, MaxChunks: -1}, wantErr: true},
		{name: "negative max source bytes", store: store, config: index.BuilderConfig{Mode: index.SemanticOff, MaxSourceBytes: -1}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder, err := index.NewBuilder(tt.store, tt.embedder, tt.config)
			if tt.wantErr {
				if builder != nil || !errors.Is(err, index.ErrInvalidBuilder) {
					t.Fatalf("NewBuilder() = %#v, %v, want nil ErrInvalidBuilder", builder, err)
				}
				return
			}
			if err != nil || builder == nil {
				t.Fatalf("NewBuilder() = %#v, %v, want builder", builder, err)
			}
		})
	}
}

func TestBuilderSemanticModesClassifyProviderFailures(t *testing.T) {
	providerCanary := "PRIVATE_PROVIDER_FAILURE_CANARY"
	tests := []struct {
		name             string
		mode             index.SemanticMode
		embedErr         error
		cancelDuringCall bool
		wantPublished    bool
		wantSemantic     string
		wantReason       string
		wantError        error
	}{
		{
			name:          "preferred degrades exhausted temporary failure",
			mode:          index.SemanticPreferred,
			embedErr:      fmt.Errorf("%s: %w", providerCanary, embedding.ErrSemanticUnavailable),
			wantPublished: true,
			wantSemantic:  "degraded",
			wantReason:    "semantic-unavailable",
		},
		{
			name:      "required preserves exhausted temporary category",
			mode:      index.SemanticRequired,
			embedErr:  fmt.Errorf("%s: %w", providerCanary, embedding.ErrSemanticUnavailable),
			wantError: embedding.ErrSemanticUnavailable,
		},
		{
			name:          "preferred degrades typed internal timeout",
			mode:          index.SemanticPreferred,
			embedErr:      fmt.Errorf("%s: %w: %w", providerCanary, embedding.ErrSemanticUnavailable, context.DeadlineExceeded),
			wantPublished: true,
			wantSemantic:  "degraded",
			wantReason:    "semantic-unavailable",
		},
		{
			name:      "preferred rejects raw internal deadline",
			mode:      index.SemanticPreferred,
			embedErr:  fmt.Errorf("%s: %w", providerCanary, context.DeadlineExceeded),
			wantError: index.ErrSemanticFailure,
		},
		{
			name:      "preferred rejects unknown provider failure",
			mode:      index.SemanticPreferred,
			embedErr:  errors.New(providerCanary),
			wantError: index.ErrSemanticFailure,
		},
		{
			name:      "required rejects unknown provider failure",
			mode:      index.SemanticRequired,
			embedErr:  errors.New(providerCanary),
			wantError: index.ErrSemanticFailure,
		},
		{
			name:             "parent cancellation never degrades",
			mode:             index.SemanticPreferred,
			embedErr:         fmt.Errorf("%s: %w", providerCanary, embedding.ErrSemanticUnavailable),
			cancelDuringCall: true,
			wantError:        context.Canceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			corpus := mustBuilderCorpus(t, "scanner-v4", []source.Chunk{
				builderChunk("chunk", "private.py", "PRIVATE_SOURCE_CANARY"),
			})
			store := &builderStore{activeErr: index.ErrNotFound}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			embedder := &builderEmbedder{
				profile: embedding.Profile{Fingerprint: "profile", Model: "model"},
				err:     tt.embedErr,
			}
			if tt.cancelDuringCall {
				embedder.onEmbed = cancel
			}
			builder, err := index.NewBuilder(store, embedder, index.BuilderConfig{Mode: tt.mode})
			if err != nil {
				t.Fatalf("NewBuilder() error = %v", err)
			}

			report, err := builder.Replace(ctx, "repository", corpus)
			if tt.wantError != nil {
				if !errors.Is(err, tt.wantError) {
					t.Fatalf("Replace() error = %v, want %v", err, tt.wantError)
				}
				if report != (index.Report{}) {
					t.Fatalf("Replace() report = %#v, want zero on unpublished failure", report)
				}
			} else if err != nil {
				t.Fatalf("Replace() error = %v", err)
			}
			if got := store.replaceCallCount(); got != boolInt(tt.wantPublished) {
				t.Fatalf("Store.Replace() calls = %d, want published %v", got, tt.wantPublished)
			}
			if report.Semantic != tt.wantSemantic || report.DegradedReason != tt.wantReason {
				t.Fatalf("Replace() semantic report = %q/%q, want %q/%q", report.Semantic, report.DegradedReason, tt.wantSemantic, tt.wantReason)
			}
			if tt.wantPublished {
				generation, _ := store.publishedGeneration()
				if generation.Profile != nil || generation.Dimensions != 0 || len(generation.Vectors) != 0 {
					t.Fatalf("degraded generation contains semantic metadata: %#v", generation)
				}
			}
			if calls, purpose, texts := embedder.calls(); calls != 1 || purpose != embedding.PurposeDocument || !reflect.DeepEqual(texts, []string{"PRIVATE_SOURCE_CANARY"}) {
				t.Fatalf("Embed() = calls %d purpose %q texts %#v", calls, purpose, texts)
			}
			for _, canary := range []string{providerCanary, "PRIVATE_SOURCE_CANARY", "private.py", "profile", "model"} {
				if errorTreeContains(err, canary) {
					t.Fatalf("Replace() error tree exposes %q", canary)
				}
				if strings.Contains(fmt.Sprintf("%+v", report), canary) {
					t.Fatalf("Replace() report exposes %q", canary)
				}
			}
		})
	}
}

func TestBuilderSemanticGenerationPreservesCanonicalOrderAndUsage(t *testing.T) {
	corpus := mustBuilderCorpus(t, "scanner-v4", []source.Chunk{
		builderChunk("first", "first.py", "first text"),
		builderChunk("second", "second.py", "second text"),
	})
	profile := embedding.Profile{Fingerprint: "fingerprint", Model: "model"}
	returnedVectors := []embedding.Vector{{1, 0}, {0, 2}}
	embedder := &builderEmbedder{
		profile: profile,
		batch: embedding.Batch{
			Profile:     profile,
			Dimensions:  2,
			Vectors:     returnedVectors,
			Requests:    2,
			UsageTokens: 17,
		},
	}
	store := &builderStore{active: "previous-generation", retainInput: true}
	builder, err := index.NewBuilder(store, embedder, index.BuilderConfig{Mode: index.SemanticRequired})
	if err != nil {
		t.Fatalf("NewBuilder() error = %v", err)
	}

	report, err := builder.Replace(context.Background(), "repository", corpus)
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	generation, _ := store.publishedGeneration()
	if generation.Profile == nil || *generation.Profile != profile || generation.Dimensions != 2 || generation.Metric != index.VectorMetricCosine {
		t.Fatalf("semantic metadata = profile %#v dimensions %d metric %q", generation.Profile, generation.Dimensions, generation.Metric)
	}
	wantRecords := []index.VectorRecord{
		{ChunkID: "first", Values: embedding.Vector{1, 0}},
		{ChunkID: "second", Values: embedding.Vector{0, 2}},
	}
	if !reflect.DeepEqual(generation.Vectors, wantRecords) {
		t.Fatalf("published vectors = %#v, want positional association %#v", generation.Vectors, wantRecords)
	}
	if calls, purpose, texts := embedder.calls(); calls != 1 || purpose != embedding.PurposeDocument || !reflect.DeepEqual(texts, []string{"first text", "second text"}) {
		t.Fatalf("Embed() = calls %d purpose %q texts %#v", calls, purpose, texts)
	}
	wantReport := index.Report{
		GenerationID:   generation.ID,
		CorpusRevision: corpus.Revision,
		Chunks:         2,
		Vectors:        2,
		Requests:       2,
		UsageTokens:    17,
		Semantic:       "indexed",
	}
	if !reflect.DeepEqual(report, wantReport) {
		t.Fatalf("Replace() report = %#v, want %#v", report, wantReport)
	}

	corpus.Chunks[0].Text = "mutated caller text"
	returnedVectors[0][0] = 99
	retained, _ := store.publishedGeneration()
	if retained.Chunks[0].Text != "first text" || retained.Vectors[0].Values[0] != 1 {
		t.Fatalf("published generation changed after caller mutation: %#v", retained)
	}
}

func TestBuilderRejectsMalformedEmbeddingBatches(t *testing.T) {
	validProfile := embedding.Profile{Fingerprint: "fingerprint", Model: "model"}
	tests := []struct {
		name           string
		mutate         func(*builderEmbedder)
		wantEmbedCalls int
	}{
		{
			name: "empty embedder fingerprint",
			mutate: func(embedder *builderEmbedder) {
				embedder.profile.Fingerprint = ""
			},
		},
		{
			name: "empty embedder model",
			mutate: func(embedder *builderEmbedder) {
				embedder.profile.Model = " "
			},
		},
		{
			name: "batch profile mismatch",
			mutate: func(embedder *builderEmbedder) {
				embedder.batch.Profile.Fingerprint = "different"
			},
			wantEmbedCalls: 1,
		},
		{
			name: "empty batch profile",
			mutate: func(embedder *builderEmbedder) {
				embedder.batch.Profile = embedding.Profile{}
			},
			wantEmbedCalls: 1,
		},
		{
			name: "missing vector",
			mutate: func(embedder *builderEmbedder) {
				embedder.batch.Vectors = nil
			},
			wantEmbedCalls: 1,
		},
		{
			name: "extra vector",
			mutate: func(embedder *builderEmbedder) {
				embedder.batch.Vectors = append(embedder.batch.Vectors, embedding.Vector{0, 1})
			},
			wantEmbedCalls: 1,
		},
		{
			name: "zero dimensions",
			mutate: func(embedder *builderEmbedder) {
				embedder.batch.Dimensions = 0
			},
			wantEmbedCalls: 1,
		},
		{
			name: "dimension mismatch",
			mutate: func(embedder *builderEmbedder) {
				embedder.batch.Dimensions = 3
			},
			wantEmbedCalls: 1,
		},
		{
			name: "NaN component",
			mutate: func(embedder *builderEmbedder) {
				embedder.batch.Vectors[0][0] = float32(math.NaN())
			},
			wantEmbedCalls: 1,
		},
		{
			name: "infinite component",
			mutate: func(embedder *builderEmbedder) {
				embedder.batch.Vectors[0][0] = float32(math.Inf(1))
			},
			wantEmbedCalls: 1,
		},
		{
			name: "zero vector",
			mutate: func(embedder *builderEmbedder) {
				embedder.batch.Vectors[0] = embedding.Vector{0, 0}
			},
			wantEmbedCalls: 1,
		},
		{
			name: "zero requests",
			mutate: func(embedder *builderEmbedder) {
				embedder.batch.Requests = 0
			},
			wantEmbedCalls: 1,
		},
		{
			name: "negative requests",
			mutate: func(embedder *builderEmbedder) {
				embedder.batch.Requests = -1
			},
			wantEmbedCalls: 1,
		},
		{
			name: "overflowed negative usage",
			mutate: func(embedder *builderEmbedder) {
				embedder.batch.UsageTokens = math.MinInt
			},
			wantEmbedCalls: 1,
		},
	}

	for _, mode := range []index.SemanticMode{index.SemanticPreferred, index.SemanticRequired} {
		for _, tt := range tests {
			t.Run(string(mode)+"/"+tt.name, func(t *testing.T) {
				corpus := mustBuilderCorpus(t, "scanner-v4", []source.Chunk{
					builderChunk("chunk", "PRIVATE_BATCH_PATH_CANARY.py", "PRIVATE_BATCH_TEXT_CANARY"),
				})
				embedder := &builderEmbedder{
					profile: validProfile,
					batch: embedding.Batch{
						Profile:     validProfile,
						Dimensions:  2,
						Vectors:     []embedding.Vector{{1, 0}},
						Requests:    1,
						UsageTokens: 1,
					},
				}
				tt.mutate(embedder)
				store := &builderStore{activeErr: index.ErrNotFound}
				builder, err := index.NewBuilder(store, embedder, index.BuilderConfig{Mode: mode})
				if err != nil {
					t.Fatalf("NewBuilder() error = %v", err)
				}

				report, err := builder.Replace(context.Background(), "repository", corpus)
				if !errors.Is(err, index.ErrSemanticIntegrity) {
					t.Fatalf("Replace() error = %v, want ErrSemanticIntegrity", err)
				}
				if report != (index.Report{}) || store.replaceCallCount() != 0 {
					t.Fatalf("malformed batch published report %#v with %d store calls", report, store.replaceCallCount())
				}
				if calls, _, _ := embedder.calls(); calls != tt.wantEmbedCalls {
					t.Fatalf("Embed() calls = %d, want %d", calls, tt.wantEmbedCalls)
				}
				for _, canary := range []string{"PRIVATE_BATCH_PATH_CANARY", "PRIVATE_BATCH_TEXT_CANARY", "fingerprint", "model"} {
					if errorTreeContains(err, canary) {
						t.Fatalf("Replace() error tree exposes %q", canary)
					}
				}
			})
		}
	}
}

func TestBuilderGenerationIDIsDeterministicAndSpaceBound(t *testing.T) {
	baseCorpus := mustBuilderCorpus(t, "scanner-v4", []source.Chunk{
		builderChunk("chunk", "chunk.py", "chunk text"),
	})
	otherRevision := mustBuilderCorpus(t, "scanner-v4", []source.Chunk{
		builderChunk("other", "chunk.py", "chunk text"),
	})
	otherPolicy := mustBuilderCorpus(t, "scanner-v5", []source.Chunk{
		builderChunk("chunk", "chunk.py", "chunk text"),
	})

	offID := buildGenerationID(t, index.SemanticOff, baseCorpus, embedding.Profile{}, nil, "")
	offAgainID := buildGenerationID(t, index.SemanticOff, baseCorpus, embedding.Profile{}, nil, "previous")
	degradedID := buildGenerationID(
		t,
		index.SemanticPreferred,
		baseCorpus,
		embedding.Profile{Fingerprint: "fingerprint", Model: "model"},
		embedding.ErrSemanticUnavailable,
		"",
	)
	semanticID := buildGenerationID(
		t,
		index.SemanticRequired,
		baseCorpus,
		embedding.Profile{Fingerprint: "fingerprint", Model: "model"},
		nil,
		"",
	)
	semanticPreferredID := buildGenerationID(
		t,
		index.SemanticPreferred,
		baseCorpus,
		embedding.Profile{Fingerprint: "fingerprint", Model: "model"},
		nil,
		"",
	)
	modelOnlyChangeID := buildGenerationID(
		t,
		index.SemanticRequired,
		baseCorpus,
		embedding.Profile{Fingerprint: "fingerprint", Model: "other-model"},
		nil,
		"",
	)
	otherFingerprintID := buildGenerationID(
		t,
		index.SemanticRequired,
		baseCorpus,
		embedding.Profile{Fingerprint: "other-fingerprint", Model: "model"},
		nil,
		"",
	)
	otherRevisionID := buildGenerationID(t, index.SemanticOff, otherRevision, embedding.Profile{}, nil, "")
	otherPolicyID := buildGenerationID(t, index.SemanticOff, otherPolicy, embedding.Profile{}, nil, "")

	if offID != offAgainID {
		t.Fatalf("same lexical space ID changed with active base: %q != %q", offID, offAgainID)
	}
	if degradedID != offID {
		t.Fatalf("degraded lexical-only ID = %q, want off ID %q", degradedID, offID)
	}
	if semanticID != semanticPreferredID {
		t.Fatalf("same semantic space differs by policy mode: required %q preferred %q", semanticID, semanticPreferredID)
	}
	if modelOnlyChangeID != semanticID {
		t.Fatalf("model-only change altered fingerprint-bound ID: %q != %q", modelOnlyChangeID, semanticID)
	}
	for name, different := range map[string]string{
		"semantic space":     semanticID,
		"fingerprint":        otherFingerprintID,
		"corpus revision":    otherRevisionID,
		"scanner policy/rev": otherPolicyID,
	} {
		if different == offID {
			t.Errorf("%s ID = %q, want separation from lexical base", name, different)
		}
	}
	if otherFingerprintID == semanticID {
		t.Errorf("other fingerprint ID = %q, want different semantic space", otherFingerprintID)
	}
}

func TestBuilderReembedsAllChunksOnEveryReplace(t *testing.T) {
	corpus := mustBuilderCorpus(t, "scanner-v4", []source.Chunk{
		builderChunk("stable-one", "one.py", "one"),
		builderChunk("stable-two", "two.py", "two"),
	})
	profile := embedding.Profile{Fingerprint: "fingerprint", Model: "model"}
	embedder := &builderEmbedder{
		profile: profile,
		batch: embedding.Batch{
			Profile:    profile,
			Dimensions: 2,
			Vectors:    []embedding.Vector{{1, 0}, {0, 1}},
			Requests:   1,
		},
	}
	store := &builderStore{active: "previous"}
	builder, err := index.NewBuilder(store, embedder, index.BuilderConfig{Mode: index.SemanticRequired})
	if err != nil {
		t.Fatalf("NewBuilder() error = %v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := builder.Replace(context.Background(), "repository", corpus); err != nil {
			t.Fatalf("Replace(attempt %d) error = %v", attempt, err)
		}
	}
	if calls, purpose, texts := embedder.calls(); calls != 2 || purpose != embedding.PurposeDocument || !reflect.DeepEqual(texts, []string{"one", "two"}) {
		t.Fatalf("Embed() = calls %d purpose %q last texts %#v, want two complete rebuilds", calls, purpose, texts)
	}
}

func TestBuilderCostLimitsAreCheckedBeforeStoreAndEmbedder(t *testing.T) {
	defaultChunkBoundary := make([]source.Chunk, 20_000)
	for position := range defaultChunkBoundary {
		id := fmt.Sprintf("chunk-%05d", position)
		defaultChunkBoundary[position] = builderChunk(id, id+".py", "x")
	}
	defaultChunkBreach := append(append([]source.Chunk(nil), defaultChunkBoundary...), builderChunk("chunk-over-limit", "over.py", "x"))
	largeText := strings.Repeat("x", 64*1024*1024)

	tests := []struct {
		name          string
		config        index.BuilderConfig
		chunks        []source.Chunk
		wantCostError bool
	}{
		{
			name:   "custom chunk boundary",
			config: index.BuilderConfig{Mode: index.SemanticOff, MaxChunks: 2},
			chunks: []source.Chunk{builderChunk("one", "one.py", "x"), builderChunk("two", "two.py", "x")},
		},
		{
			name:          "custom chunk breach",
			config:        index.BuilderConfig{Mode: index.SemanticPreferred, MaxChunks: 1},
			chunks:        []source.Chunk{builderChunk("one", "one.py", "x"), builderChunk("two", "two.py", "x")},
			wantCostError: true,
		},
		{
			name:   "custom byte boundary",
			config: index.BuilderConfig{Mode: index.SemanticOff, MaxSourceBytes: 4},
			chunks: []source.Chunk{builderChunk("one", "one.py", "ab"), builderChunk("two", "two.py", "cd")},
		},
		{
			name:          "overflow-safe byte breach",
			config:        index.BuilderConfig{Mode: index.SemanticPreferred, MaxSourceBytes: 3},
			chunks:        []source.Chunk{builderChunk("one", "one.py", "ab"), builderChunk("two", "two.py", "cd")},
			wantCostError: true,
		},
		{
			name:   "default chunk boundary",
			config: index.BuilderConfig{Mode: index.SemanticOff},
			chunks: defaultChunkBoundary,
		},
		{
			name:          "default chunk breach",
			config:        index.BuilderConfig{Mode: index.SemanticPreferred},
			chunks:        defaultChunkBreach,
			wantCostError: true,
		},
		{
			name:   "default source byte boundary",
			config: index.BuilderConfig{Mode: index.SemanticOff},
			chunks: []source.Chunk{builderChunk("large", "large.py", largeText)},
		},
		{
			name:          "default source byte breach",
			config:        index.BuilderConfig{Mode: index.SemanticPreferred},
			chunks:        []source.Chunk{builderChunk("large", "large.py", largeText), builderChunk("extra", "extra.py", "x")},
			wantCostError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			corpus := mustBuilderCorpus(t, "scanner-v4", tt.chunks)
			store := &builderStore{activeErr: index.ErrNotFound}
			embedder := &builderEmbedder{profile: embedding.Profile{Fingerprint: "fingerprint", Model: "model"}}
			builder, err := index.NewBuilder(store, embedder, tt.config)
			if err != nil {
				t.Fatalf("NewBuilder() error = %v", err)
			}

			report, err := builder.Replace(context.Background(), "repository", corpus)
			if tt.wantCostError {
				if !errors.Is(err, index.ErrCostLimit) {
					t.Fatalf("Replace() error = %v, want ErrCostLimit", err)
				}
				if report != (index.Report{}) || store.activeCallCount() != 0 || store.replaceCallCount() != 0 {
					t.Fatalf("cost breach crossed store boundary: report %#v active %d replace %d", report, store.activeCallCount(), store.replaceCallCount())
				}
				if calls, _, _ := embedder.calls(); calls != 0 {
					t.Fatalf("cost breach Embed() calls = %d, want 0", calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("Replace(boundary) error = %v", err)
			}
			if report.Chunks != len(tt.chunks) || store.replaceCallCount() != 1 {
				t.Fatalf("boundary report/store = %#v/%d", report, store.replaceCallCount())
			}
		})
	}
}

func TestBuilderRejectsInvalidRequestsBeforeStoreAndEmbedder(t *testing.T) {
	validCorpus := mustBuilderCorpus(t, "scanner-v4", []source.Chunk{
		builderChunk("chunk", "PRIVATE_REQUEST_PATH_CANARY.py", "PRIVATE_REQUEST_TEXT_CANARY"),
	})
	forgedRevision := validCorpus
	forgedRevision.Revision = "PRIVATE_FORGED_REVISION_CANARY"
	missingPolicy := validCorpus
	missingPolicy.PolicyVersion = ""
	duplicate := validCorpus
	duplicate.Chunks = append(append([]source.Chunk(nil), validCorpus.Chunks...), validCorpus.Chunks[0])
	incomplete := validCorpus
	incomplete.Chunks = append([]source.Chunk(nil), validCorpus.Chunks...)
	incomplete.Chunks[0].Reference.Path = "../PRIVATE_INVALID_PATH_CANARY.py"
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name       string
		ctx        context.Context
		repository string
		corpus     source.Corpus
		nilBuilder bool
		want       error
	}{
		{name: "nil builder", ctx: context.Background(), repository: "repository", corpus: validCorpus, nilBuilder: true, want: index.ErrInvalidBuilder},
		{name: "nil context", repository: "repository", corpus: validCorpus, want: index.ErrInvalidBuilder},
		{name: "canceled context", ctx: canceled, repository: "repository", corpus: validCorpus, want: context.Canceled},
		{name: "empty repository", ctx: context.Background(), repository: " ", corpus: validCorpus, want: index.ErrInvalidCorpus},
		{name: "forged revision", ctx: context.Background(), repository: "repository", corpus: forgedRevision, want: index.ErrInvalidCorpus},
		{name: "missing policy", ctx: context.Background(), repository: "repository", corpus: missingPolicy, want: index.ErrInvalidCorpus},
		{name: "duplicate chunk", ctx: context.Background(), repository: "repository", corpus: duplicate, want: index.ErrInvalidCorpus},
		{name: "invalid reference", ctx: context.Background(), repository: "repository", corpus: incomplete, want: index.ErrInvalidCorpus},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &builderStore{activeErr: index.ErrNotFound}
			embedder := &builderEmbedder{profile: embedding.Profile{Fingerprint: "fingerprint", Model: "model"}}
			var builder *index.Builder
			if !tt.nilBuilder {
				var err error
				builder, err = index.NewBuilder(store, embedder, index.BuilderConfig{Mode: index.SemanticPreferred})
				if err != nil {
					t.Fatalf("NewBuilder() error = %v", err)
				}
			}

			report, err := builder.Replace(tt.ctx, tt.repository, tt.corpus)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Replace() error = %v, want %v", err, tt.want)
			}
			if report != (index.Report{}) || store.activeCallCount() != 0 || store.replaceCallCount() != 0 {
				t.Fatalf("invalid request crossed store boundary: report %#v active %d replace %d", report, store.activeCallCount(), store.replaceCallCount())
			}
			if calls, _, _ := embedder.calls(); calls != 0 {
				t.Fatalf("invalid request Embed() calls = %d, want 0", calls)
			}
			for _, canary := range []string{"PRIVATE_REQUEST_PATH_CANARY", "PRIVATE_REQUEST_TEXT_CANARY", "PRIVATE_FORGED_REVISION_CANARY", "PRIVATE_INVALID_PATH_CANARY"} {
				if errorTreeContains(err, canary) {
					t.Fatalf("Replace() error tree exposes %q", canary)
				}
			}
		})
	}
}

func TestBuilderSanitizesActiveGenerationErrors(t *testing.T) {
	activeCanary := "PRIVATE_ACTIVE_STORE_CANARY"
	tests := []struct {
		name      string
		activeErr error
		want      error
	}{
		{name: "unknown", activeErr: errors.New(activeCanary), want: index.ErrStoreFailure},
		{name: "canceled", activeErr: fmt.Errorf("%s: %w", activeCanary, context.Canceled), want: context.Canceled},
		{name: "deadline", activeErr: fmt.Errorf("%s: %w", activeCanary, context.DeadlineExceeded), want: context.DeadlineExceeded},
		{name: "concurrent", activeErr: fmt.Errorf("%s: %w", activeCanary, index.ErrConcurrentIndex), want: index.ErrConcurrentIndex},
		{name: "invalid generation", activeErr: fmt.Errorf("%s: %w", activeCanary, index.ErrInvalidGeneration), want: index.ErrInvalidGeneration},
		{name: "reindex required", activeErr: fmt.Errorf("%s: %w", activeCanary, index.ErrReindexRequired), want: index.ErrReindexRequired},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			corpus := mustBuilderCorpus(t, "scanner-v4", []source.Chunk{builderChunk("chunk", "active.py", "text")})
			store := &builderStore{activeErr: tt.activeErr}
			builder, err := index.NewBuilder(store, nil, index.BuilderConfig{Mode: index.SemanticOff})
			if err != nil {
				t.Fatalf("NewBuilder() error = %v", err)
			}

			report, err := builder.Replace(context.Background(), "repository", corpus)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Replace() error = %v, want %v", err, tt.want)
			}
			if report != (index.Report{}) || store.activeCallCount() != 1 || store.replaceCallCount() != 0 {
				t.Fatalf("active failure outcome = report %#v active %d replace %d", report, store.activeCallCount(), store.replaceCallCount())
			}
			if errorTreeContains(err, activeCanary) {
				t.Fatalf("Replace() error tree exposes active store canary")
			}
		})
	}
}

func TestBuilderPreservesPublicationOutcomesWithoutRetry(t *testing.T) {
	publicationCanary := "PRIVATE_PUBLICATION_STORE_CANARY"
	committed := index.NewCommittedCleanupError(index.CleanupStageCheckpoint)
	tests := []struct {
		name       string
		replaceErr error
		want       error
		committed  bool
	}{
		{name: "concurrent", replaceErr: fmt.Errorf("%s: %w", publicationCanary, index.ErrConcurrentIndex), want: index.ErrConcurrentIndex},
		{name: "unknown", replaceErr: errors.New(publicationCanary), want: index.ErrStoreFailure},
		{name: "invalid generation", replaceErr: fmt.Errorf("%s: %w", publicationCanary, index.ErrInvalidGeneration), want: index.ErrInvalidGeneration},
		{name: "reindex required", replaceErr: fmt.Errorf("%s: %w", publicationCanary, index.ErrReindexRequired), want: index.ErrReindexRequired},
		{name: "committed cleanup", replaceErr: fmt.Errorf("%s: %w", publicationCanary, committed), want: index.ErrCleanupIncomplete, committed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			corpus := mustBuilderCorpus(t, "scanner-v4", []source.Chunk{builderChunk("chunk", "publication.py", "text")})
			store := &builderStore{activeErr: index.ErrNotFound, replaceErr: tt.replaceErr}
			builder, err := index.NewBuilder(store, nil, index.BuilderConfig{Mode: index.SemanticOff})
			if err != nil {
				t.Fatalf("NewBuilder() error = %v", err)
			}

			report, err := builder.Replace(context.Background(), "repository", corpus)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Replace() error = %v, want %v", err, tt.want)
			}
			if store.replaceCallCount() != 1 {
				t.Fatalf("Store.Replace() calls = %d, want no retry", store.replaceCallCount())
			}
			if tt.committed {
				if report.GenerationID == "" || report.CorpusRevision != corpus.Revision || report.Chunks != 1 {
					t.Fatalf("committed report = %#v, want published generation", report)
				}
				var outcome *index.CommittedCleanupError
				if !errors.As(err, &outcome) || !outcome.Published() || outcome.Stage() != index.CleanupStageCheckpoint {
					t.Fatalf("committed outcome = %T %#v", err, outcome)
				}
			} else if report != (index.Report{}) {
				t.Fatalf("unpublished report = %#v, want zero", report)
			}
			if errorTreeContains(err, publicationCanary) {
				t.Fatalf("Replace() error tree exposes publication canary")
			}
		})
	}
}

func TestBuilderChecksCancellationAfterActiveGenerationAndEmbedding(t *testing.T) {
	corpus := mustBuilderCorpus(t, "scanner-v4", []source.Chunk{builderChunk("chunk", "cancel.py", "text")})
	profile := embedding.Profile{Fingerprint: "fingerprint", Model: "model"}
	tests := []struct {
		name           string
		cancelOnActive bool
		cancelOnEmbed  bool
		wantEmbeds     int
	}{
		{name: "after active generation", cancelOnActive: true},
		{name: "after embedding", cancelOnEmbed: true, wantEmbeds: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := &builderStore{activeErr: index.ErrNotFound}
			if tt.cancelOnActive {
				store.onActive = cancel
			}
			embedder := &builderEmbedder{
				profile: profile,
				batch:   embedding.Batch{Profile: profile, Dimensions: 1, Vectors: []embedding.Vector{{1}}, Requests: 1},
			}
			if tt.cancelOnEmbed {
				embedder.onEmbed = cancel
			}
			builder, err := index.NewBuilder(store, embedder, index.BuilderConfig{Mode: index.SemanticPreferred})
			if err != nil {
				t.Fatalf("NewBuilder() error = %v", err)
			}

			report, err := builder.Replace(ctx, "repository", corpus)
			if !errors.Is(err, context.Canceled) || report != (index.Report{}) {
				t.Fatalf("Replace() = %#v, %v, want canceled unpublished", report, err)
			}
			if calls, _, _ := embedder.calls(); calls != tt.wantEmbeds {
				t.Fatalf("Embed() calls = %d, want %d", calls, tt.wantEmbeds)
			}
			if store.replaceCallCount() != 0 {
				t.Fatalf("Store.Replace() calls = %d, want 0", store.replaceCallCount())
			}
		})
	}
}

func TestBuilderSupportsConcurrentIndependentReplacements(t *testing.T) {
	corpus := mustBuilderCorpus(t, "scanner-v4", []source.Chunk{builderChunk("chunk", "concurrent.py", "text")})
	profile := embedding.Profile{Fingerprint: "fingerprint", Model: "model"}
	embedder := &builderEmbedder{
		profile: profile,
		batch:   embedding.Batch{Profile: profile, Dimensions: 1, Vectors: []embedding.Vector{{1}}, Requests: 1},
	}
	store := &builderStore{activeErr: index.ErrNotFound}
	builder, err := index.NewBuilder(store, embedder, index.BuilderConfig{Mode: index.SemanticRequired})
	if err != nil {
		t.Fatalf("NewBuilder() error = %v", err)
	}

	const workers = 16
	start := make(chan struct{})
	errorsSeen := make(chan error, workers)
	reports := make(chan index.Report, workers)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(position int) {
			defer group.Done()
			<-start
			report, replaceErr := builder.Replace(context.Background(), fmt.Sprintf("repository-%d", position), corpus)
			errorsSeen <- replaceErr
			reports <- report
		}(worker)
	}
	close(start)
	group.Wait()
	close(errorsSeen)
	close(reports)
	for replaceErr := range errorsSeen {
		if replaceErr != nil {
			t.Fatalf("concurrent Replace() error = %v", replaceErr)
		}
	}
	var generationID string
	for report := range reports {
		if generationID == "" {
			generationID = report.GenerationID
		}
		if report.GenerationID != generationID || report.Semantic != "indexed" {
			t.Fatalf("concurrent report = %#v, want deterministic indexed generation", report)
		}
	}
	if calls, _, _ := embedder.calls(); calls != workers {
		t.Fatalf("Embed() calls = %d, want %d", calls, workers)
	}
	if store.activeCallCount() != workers || store.replaceCallCount() != workers {
		t.Fatalf("store calls = active %d replace %d, want %d each", store.activeCallCount(), store.replaceCallCount(), workers)
	}
}

type builderStore struct {
	mu           sync.Mutex
	active       string
	activeErr    error
	replaceErr   error
	generation   index.Generation
	activeCalls  int
	replaceCalls int
	sequence     []string
	retainInput  bool
	onActive     func()
	onReplace    func()
}

func (s *builderStore) ActiveGeneration(ctx context.Context, _ string) (string, error) {
	s.mu.Lock()
	s.activeCalls++
	s.sequence = append(s.sequence, "active")
	active, activeErr, onActive := s.active, s.activeErr, s.onActive
	s.mu.Unlock()
	if onActive != nil {
		onActive()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return active, activeErr
}

func (s *builderStore) Replace(ctx context.Context, generation index.Generation) error {
	s.mu.Lock()
	s.replaceCalls++
	s.sequence = append(s.sequence, "replace")
	if s.retainInput {
		s.generation = generation
	} else {
		s.generation = cloneBuilderGeneration(generation)
	}
	replaceErr, onReplace := s.replaceErr, s.onReplace
	s.mu.Unlock()
	if onReplace != nil {
		onReplace()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return replaceErr
}

func (s *builderStore) replaceCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replaceCalls
}

func (s *builderStore) activeCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeCalls
}

func (s *builderStore) publishedGeneration() (index.Generation, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneBuilderGeneration(s.generation), s.replaceCalls
}

func (s *builderStore) callSequence() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sequence...)
}

type builderEmbedder struct {
	mu        sync.Mutex
	profile   embedding.Profile
	batch     embedding.Batch
	err       error
	onEmbed   func()
	callCount int
	purpose   embedding.Purpose
	texts     []string
}

func (e *builderEmbedder) Profile() embedding.Profile {
	return e.profile
}

func (e *builderEmbedder) Embed(_ context.Context, purpose embedding.Purpose, texts []string) (embedding.Batch, error) {
	e.mu.Lock()
	e.callCount++
	e.purpose = purpose
	e.texts = append([]string(nil), texts...)
	onEmbed := e.onEmbed
	batch, err := e.batch, e.err
	e.mu.Unlock()
	if onEmbed != nil {
		onEmbed()
	}
	return batch, err
}

func (e *builderEmbedder) calls() (int, embedding.Purpose, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.callCount, e.purpose, append([]string(nil), e.texts...)
}

func mustBuilderCorpus(t *testing.T, policy string, chunks []source.Chunk) source.Corpus {
	t.Helper()
	corpus, err := source.NewCorpus(policy, chunks)
	if err != nil {
		t.Fatalf("source.NewCorpus() error = %v", err)
	}
	return corpus
}

func builderChunk(id, path, text string) source.Chunk {
	return source.Chunk{
		ID:         id,
		Text:       text,
		Language:   source.LanguagePython,
		SymbolName: "Value",
		Reference: source.Reference{
			Path:      path,
			StartLine: 1,
			EndLine:   1,
		},
	}
}

func buildGenerationID(
	t *testing.T,
	mode index.SemanticMode,
	corpus source.Corpus,
	profile embedding.Profile,
	embedErr error,
	active string,
) string {
	t.Helper()
	store := &builderStore{active: active}
	if active == "" {
		store.activeErr = index.ErrNotFound
	}
	var embedder embedding.Embedder
	if mode != index.SemanticOff {
		vectors := make([]embedding.Vector, len(corpus.Chunks))
		for position := range vectors {
			vectors[position] = embedding.Vector{1}
		}
		embedder = &builderEmbedder{
			profile: profile,
			batch: embedding.Batch{
				Profile:    profile,
				Dimensions: 1,
				Vectors:    vectors,
				Requests:   1,
			},
			err: embedErr,
		}
	}
	builder, err := index.NewBuilder(store, embedder, index.BuilderConfig{Mode: mode})
	if err != nil {
		t.Fatalf("NewBuilder() error = %v", err)
	}
	report, err := builder.Replace(context.Background(), "repository", corpus)
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if report.GenerationID == "" {
		t.Fatal("Replace() generation ID is empty")
	}
	return report.GenerationID
}

func cloneBuilderGeneration(generation index.Generation) index.Generation {
	clone := generation
	clone.Chunks = append([]source.Chunk(nil), generation.Chunks...)
	if generation.Profile != nil {
		profile := *generation.Profile
		clone.Profile = &profile
	}
	clone.Vectors = make([]index.VectorRecord, len(generation.Vectors))
	for position, record := range generation.Vectors {
		clone.Vectors[position] = index.VectorRecord{
			ChunkID: record.ChunkID,
			Values:  append(embedding.Vector(nil), record.Values...),
		}
	}
	return clone
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func errorTreeContains(err error, value string) bool {
	if err == nil {
		return false
	}
	if strings.Contains(err.Error(), value) {
		return true
	}
	if multiple, ok := err.(interface{ Unwrap() []error }); ok {
		for _, nested := range multiple.Unwrap() {
			if errorTreeContains(nested, value) {
				return true
			}
		}
		return false
	}
	return errorTreeContains(errors.Unwrap(err), value)
}
