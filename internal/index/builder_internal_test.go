package index

import (
	"context"
	"errors"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	"github.com/ToledoVitor/GoContext/internal/source"
)

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
