package sqlite_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	"github.com/ToledoVitor/GoContext/internal/index"
	indexsqlite "github.com/ToledoVitor/GoContext/internal/index/sqlite"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	vectorsearch "github.com/ToledoVitor/GoContext/internal/search/vector"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func BenchmarkExactSearch10000x1536(b *testing.B) {
	const (
		chunkCount = 10_000
		dimensions = 1_536
	)
	ctx := context.Background()
	chunks := make([]source.Chunk, chunkCount)
	vectors := make([]index.VectorRecord, chunkCount)
	for position := 0; position < chunkCount; position++ {
		chunkID := fmt.Sprintf("benchmark-%05d", position)
		chunks[position] = source.Chunk{
			ID: chunkID, Text: "deterministic synthetic benchmark corpus",
			Language: source.LanguagePython,
			Reference: source.Reference{
				Path:      fmt.Sprintf("synthetic/benchmark-%05d.py", position),
				StartLine: 1,
				EndLine:   1,
			},
		}
		values := make(embedding.Vector, dimensions)
		if position == 0 {
			values[0] = 1
		} else {
			values[0] = 0.8
			values[1+position%(dimensions-1)] = 0.48
			values[1+(position*31)%(dimensions-1)] = 0.36
		}
		vectors[position] = index.VectorRecord{ChunkID: chunkID, Values: values}
	}
	corpus, err := source.NewCorpus(ingest.ScanPolicyVersion, chunks)
	if err != nil {
		b.Fatalf("NewCorpus() error = %v", err)
	}
	profile := embedding.Profile{Fingerprint: "benchmark-profile-v1", Model: "synthetic-benchmark-model"}
	store, err := indexsqlite.NewStore(b.TempDir())
	if err != nil {
		b.Fatalf("NewStore() error = %v", err)
	}
	if err := store.Replace(ctx, index.Generation{
		RepositoryID:      "synthetic-benchmark-repository",
		ID:                "synthetic-benchmark-generation",
		CorpusRevision:    corpus.Revision,
		ScanPolicyVersion: corpus.PolicyVersion,
		Chunks:            chunks,
		Profile:           &profile,
		Dimensions:        dimensions,
		Metric:            index.VectorMetricCosine,
		Vectors:           vectors,
	}); err != nil {
		_ = store.Close()
		b.Fatalf("Replace() error = %v", err)
	}
	reader, err := store.BindActive(ctx, "synthetic-benchmark-repository")
	if err != nil {
		_ = store.Close()
		b.Fatalf("BindActive() error = %v", err)
	}
	queryVector := make(embedding.Vector, dimensions)
	queryVector[0] = 1
	query := vectorsearch.IndexQuery{
		RepositoryID: "synthetic-benchmark-repository",
		GenerationID: "synthetic-benchmark-generation",
		Profile:      profile,
		Dimensions:   dimensions,
		Metric:       index.VectorMetricCosine,
		Vector:       queryVector,
		Limit:        1,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		hits, err := reader.Search(ctx, query)
		if err != nil {
			b.Fatalf("Search() error = %v", err)
		}
		if len(hits) != 1 || hits[0].Chunk.ID != "benchmark-00000" {
			b.Fatalf("Search() first hit = %#v, want benchmark-00000", hits)
		}
	}
	b.StopTimer()
	if err := reader.Close(); err != nil {
		b.Fatalf("reader.Close() error = %v", err)
	}
	if err := store.Close(); err != nil {
		b.Fatalf("store.Close() error = %v", err)
	}
}
