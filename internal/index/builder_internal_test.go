package index

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestGenerationManifestDigestBindsSemanticIdentity(t *testing.T) {
	manifest := GenerationManifest{
		RepositoryID:       "repository",
		GenerationID:       strings.Repeat("a", 64),
		CorpusRevision:     strings.Repeat("b", 64),
		ContentDigest:      strings.Repeat("c", 64),
		ScanPolicyVersion:  "scanner-v4",
		ProfileFingerprint: "profile-fingerprint",
		ProfileModel:       "profile-model",
		Dimensions:         2,
		Metric:             VectorMetricCosine,
		VectorDigest:       strings.Repeat("d", 64),
	}
	want, err := GenerationManifestDigest(manifest)
	if err != nil {
		t.Fatalf("GenerationManifestDigest() error = %v", err)
	}
	if len(want) != 64 || want != strings.ToLower(want) {
		t.Fatalf("GenerationManifestDigest() = %q, want lowercase SHA-256", want)
	}
	again, err := GenerationManifestDigest(manifest)
	if err != nil || again != want {
		t.Fatalf("GenerationManifestDigest(repeat) = %q, %v; want %q", again, err, want)
	}

	mutations := []struct {
		name   string
		mutate func(*GenerationManifest)
	}{
		{name: "repository", mutate: func(value *GenerationManifest) { value.RepositoryID = "other" }},
		{name: "generation", mutate: func(value *GenerationManifest) { value.GenerationID = strings.Repeat("e", 64) }},
		{name: "revision", mutate: func(value *GenerationManifest) { value.CorpusRevision = strings.Repeat("e", 64) }},
		{name: "content", mutate: func(value *GenerationManifest) { value.ContentDigest = strings.Repeat("e", 64) }},
		{name: "policy", mutate: func(value *GenerationManifest) { value.ScanPolicyVersion = "scanner-v5" }},
		{name: "fingerprint", mutate: func(value *GenerationManifest) { value.ProfileFingerprint = "other-fingerprint" }},
		{name: "model", mutate: func(value *GenerationManifest) { value.ProfileModel = "other-model" }},
		{name: "dimensions", mutate: func(value *GenerationManifest) { value.Dimensions = 3 }},
		{name: "metric", mutate: func(value *GenerationManifest) { value.Metric = VectorMetric("other") }},
		{name: "vector digest", mutate: func(value *GenerationManifest) { value.VectorDigest = strings.Repeat("e", 64) }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := manifest
			mutation.mutate(&changed)
			got, digestErr := GenerationManifestDigest(changed)
			if mutation.name == "metric" {
				if !errors.Is(digestErr, ErrInvalidGeneration) {
					t.Fatalf("GenerationManifestDigest(mutated metric) error = %v, want ErrInvalidGeneration", digestErr)
				}
				return
			}
			if digestErr != nil {
				t.Fatalf("GenerationManifestDigest(mutated) error = %v", digestErr)
			}
			if got == want {
				t.Fatalf("GenerationManifestDigest(mutated) = unchanged %q", got)
			}
		})
	}
}

func TestGenerationManifestDigestDefinesLexicalOnlyStateAndRejectsPartialMetadata(t *testing.T) {
	lexical := GenerationManifest{
		RepositoryID:      "repository",
		GenerationID:      strings.Repeat("a", 64),
		CorpusRevision:    strings.Repeat("b", 64),
		ContentDigest:     strings.Repeat("c", 64),
		ScanPolicyVersion: "scanner-v4",
		Metric:            VectorMetricCosine,
		VectorDigest:      strings.Repeat("d", 64),
	}
	if _, err := GenerationManifestDigest(lexical); err != nil {
		t.Fatalf("GenerationManifestDigest(lexical) error = %v", err)
	}
	partial := lexical
	partial.ProfileFingerprint = "fingerprint"
	if _, err := GenerationManifestDigest(partial); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("GenerationManifestDigest(partial semantic metadata) error = %v, want ErrInvalidGeneration", err)
	}
	malformed := lexical
	malformed.VectorDigest = strings.Repeat("D", 64)
	if _, err := GenerationManifestDigest(malformed); !errors.Is(err, ErrInvalidGeneration) {
		t.Fatalf("GenerationManifestDigest(non-canonical digest) error = %v, want ErrInvalidGeneration", err)
	}
}

func TestBuilderReplaceRejectsTypedNilDependenciesWithoutCallingThem(t *testing.T) {
	corpus, err := source.NewCorpus("scanner-v4", []source.Chunk{{
		ID:        "chunk",
		Text:      "text",
		Language:  source.LanguagePython,
		Reference: source.Reference{Path: "chunk.py", StartLine: 1, EndLine: 1},
	}})
	if err != nil {
		t.Fatalf("source.NewCorpus() error = %v", err)
	}
	var typedNilStore *panickingTypedNilStore
	var typedNilEmbedder *panickingTypedNilEmbedder
	tests := []struct {
		name    string
		builder *Builder
	}{
		{
			name: "store",
			builder: &Builder{
				store:          typedNilStore,
				mode:           SemanticOff,
				maxChunks:      defaultMaxChunks,
				maxSourceBytes: defaultMaxSourceBytes,
			},
		},
		{
			name: "embedder",
			builder: &Builder{
				store:          acceptingInternalStore{},
				embedder:       typedNilEmbedder,
				mode:           SemanticRequired,
				maxChunks:      defaultMaxChunks,
				maxSourceBytes: defaultMaxSourceBytes,
			},
		},
		{
			name: "off mode typed nil embedder",
			builder: &Builder{
				store:          acceptingInternalStore{},
				embedder:       typedNilEmbedder,
				mode:           SemanticOff,
				maxChunks:      defaultMaxChunks,
				maxSourceBytes: defaultMaxSourceBytes,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := tt.builder.Replace(context.Background(), "repository", corpus)
			if !errors.Is(err, ErrInvalidBuilder) {
				t.Fatalf("Replace() error = %v, want ErrInvalidBuilder", err)
			}
			if report != (Report{}) {
				t.Fatalf("Replace() report = %#v, want zero", report)
			}
		})
	}
}

type panickingTypedNilStore struct{}

func (*panickingTypedNilStore) ActiveGeneration(context.Context, string) (string, error) {
	panic("typed nil store was called")
}

func (*panickingTypedNilStore) Replace(context.Context, Generation) error {
	panic("typed nil store was called")
}

type panickingTypedNilEmbedder struct{}

func (*panickingTypedNilEmbedder) Profile() embedding.Profile {
	panic("typed nil embedder was called")
}

func (*panickingTypedNilEmbedder) Embed(context.Context, embedding.Purpose, []string) (embedding.Batch, error) {
	panic("typed nil embedder was called")
}

type acceptingInternalStore struct{}

func (acceptingInternalStore) ActiveGeneration(context.Context, string) (string, error) {
	return "", ErrNotFound
}

func (acceptingInternalStore) Replace(context.Context, Generation) error {
	return nil
}
