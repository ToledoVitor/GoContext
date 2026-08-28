package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	"github.com/ToledoVitor/GoContext/internal/index"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/search"
	vectorsearch "github.com/ToledoVitor/GoContext/internal/search/vector"
	"github.com/ToledoVitor/GoContext/internal/source"
	modernsqlite "modernc.org/sqlite"
)

var _ vectorsearch.Index = (*BoundReader)(nil)

var canonicalChunkReadProbe atomic.Int64

func init() {
	modernsqlite.MustRegisterScalarFunction(
		"gocontext_test_canonical_chunk_read",
		1,
		func(_ *modernsqlite.FunctionContext, arguments []driver.Value) (driver.Value, error) {
			canonicalChunkReadProbe.Add(1)
			return arguments[0], nil
		},
	)
}

func TestVectorEncodingUsesLittleEndianFloat32(t *testing.T) {
	values := embedding.Vector{1, -2.5, math.SmallestNonzeroFloat32}

	encoded, err := encodeVector(values)
	if err != nil {
		t.Fatalf("encodeVector() error = %v", err)
	}
	want := []byte{
		0x00, 0x00, 0x80, 0x3f,
		0x00, 0x00, 0x20, 0xc0,
		0x01, 0x00, 0x00, 0x00,
	}
	if !reflect.DeepEqual(encoded, want) {
		t.Fatalf("encodeVector() = %x, want %x", encoded, want)
	}

	decoded, err := decodeVector(vectorEncodingVersion, len(values), encoded)
	if err != nil {
		t.Fatalf("decodeVector() error = %v", err)
	}
	if !reflect.DeepEqual(decoded, values) {
		t.Fatalf("decodeVector() = %#v, want exact float32 round trip %#v", decoded, values)
	}
}

func TestVectorEncodingRejectsInvalidValuesAndBlobs(t *testing.T) {
	nanBlob := make([]byte, 4)
	binary.LittleEndian.PutUint32(nanBlob, math.Float32bits(float32(math.NaN())))
	infBlob := make([]byte, 4)
	binary.LittleEndian.PutUint32(infBlob, math.Float32bits(float32(math.Inf(1))))
	zeroBlob := make([]byte, 8)
	validBlob := make([]byte, 8)
	binary.LittleEndian.PutUint32(validBlob, math.Float32bits(1))

	decodeCases := []struct {
		name       string
		version    int
		dimensions int
		blob       []byte
	}{
		{name: "unknown version", version: vectorEncodingVersion + 1, dimensions: 1, blob: validBlob[:4]},
		{name: "empty", version: vectorEncodingVersion, dimensions: 0, blob: nil},
		{name: "partial float", version: vectorEncodingVersion, dimensions: 1, blob: []byte{1, 2, 3}},
		{name: "dimension mismatch", version: vectorEncodingVersion, dimensions: 1, blob: validBlob},
		{name: "NaN", version: vectorEncodingVersion, dimensions: 1, blob: nanBlob},
		{name: "infinity", version: vectorEncodingVersion, dimensions: 1, blob: infBlob},
		{name: "zero", version: vectorEncodingVersion, dimensions: 2, blob: zeroBlob},
	}
	for _, tt := range decodeCases {
		t.Run("decode "+tt.name, func(t *testing.T) {
			if _, err := decodeVector(tt.version, tt.dimensions, tt.blob); err == nil {
				t.Fatal("decodeVector() error = nil, want rejection")
			}
		})
	}

	encodeCases := []embedding.Vector{
		nil,
		{0, 0},
		{float32(math.NaN())},
		{float32(math.Inf(-1))},
	}
	for _, values := range encodeCases {
		if _, err := encodeVector(values); err == nil {
			t.Fatalf("encodeVector(%#v) error = nil, want rejection", values)
		}
	}
}

func TestVectorPublicationPersistsNormalizedVersionedRows(t *testing.T) {
	directory := t.TempDir()
	store := newVectorStore(t, directory)
	generation := vectorGeneration(t, "repository", "generation", "", []source.Chunk{
		vectorChunk("first", "pkg/first.py", 1, source.LanguagePython, "FIRST_VECTOR_SOURCE_CANARY"),
		vectorChunk("second", "pkg/second.ts", 2, source.LanguageTypeScript, "SECOND_VECTOR_SOURCE_CANARY"),
	}, []index.VectorRecord{
		{ChunkID: "first", Values: embedding.Vector{3, 4}},
		{ChunkID: "second", Values: embedding.Vector{-2, 0}},
	})

	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	database, err := sql.Open("sqlite", filepath.Join(directory, databaseName))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer database.Close()
	rows, err := database.Query(`
		SELECT chunk_id, encoding_version, dimensions, values_blob
		FROM vectors
		WHERE repository_id = ? AND generation_id = ?
		ORDER BY chunk_id`, generation.RepositoryID, generation.ID)
	if err != nil {
		t.Fatalf("query vectors error = %v", err)
	}
	defer rows.Close()

	type storedRow struct {
		chunkID string
		values  embedding.Vector
	}
	var stored []storedRow
	for rows.Next() {
		var chunkID string
		var version, dimensions int
		var blob []byte
		if err := rows.Scan(&chunkID, &version, &dimensions, &blob); err != nil {
			t.Fatalf("scan vector row error = %v", err)
		}
		values, err := decodeVector(version, dimensions, blob)
		if err != nil {
			t.Fatalf("decode persisted vector error = %v", err)
		}
		stored = append(stored, storedRow{chunkID: chunkID, values: values})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read vector rows error = %v", err)
	}
	want := []storedRow{
		{chunkID: "first", values: embedding.Vector{float32(0.6), float32(0.8)}},
		{chunkID: "second", values: embedding.Vector{-1, 0}},
	}
	if !reflect.DeepEqual(stored, want) {
		t.Fatalf("stored vectors = %#v, want one normalized row per chunk %#v", stored, want)
	}
	var vectorDigest, manifestDigest string
	if err := database.QueryRow(`
		SELECT vector_digest, manifest_digest
		FROM generations
		WHERE repository_id = ? AND generation_id = ?`, generation.RepositoryID, generation.ID,
	).Scan(&vectorDigest, &manifestDigest); err != nil {
		t.Fatalf("query persisted generation digests error = %v", err)
	}
	for name, digest := range map[string]string{"vector": vectorDigest, "manifest": manifestDigest} {
		if len(digest) != 64 || digest != strings.ToLower(digest) {
			t.Fatalf("%s digest = %q, want strict lowercase SHA-256", name, digest)
		}
	}
}

func TestExactSearchRejectsSameSizeAlternateUnitVectorByPersistedDigest(t *testing.T) {
	directory := t.TempDir()
	store := newVectorStore(t, directory)
	generation := vectorGeneration(t, "repository", "generation", "", []source.Chunk{
		vectorChunk("chunk", "private/digest.py", 1, source.LanguagePython, "PRIVATE_DIGEST_SOURCE_CANARY"),
	}, []index.VectorRecord{{ChunkID: "chunk", Values: embedding.Vector{1, 0}}})
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	alternate, err := encodeVector(embedding.Vector{0, 1})
	if err != nil {
		t.Fatalf("encodeVector(alternate) error = %v", err)
	}
	database, err := sql.Open("sqlite", filepath.Join(directory, databaseName))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := database.Exec(`
		UPDATE vectors SET values_blob = ?
		WHERE repository_id = ? AND generation_id = ?`, alternate, generation.RepositoryID, generation.ID,
	); err != nil {
		_ = database.Close()
		t.Fatalf("replace vector bytes error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close mutation database error = %v", err)
	}
	reader := bindVectorReader(t, store, generation.RepositoryID)
	if _, err := reader.Search(context.Background(), vectorIndexQuery(generation, embedding.Vector{1, 0}, 1)); !errors.Is(err, vectorsearch.ErrVectorIntegrity) {
		t.Fatalf("Search(alternate unit vector) error = %v, want ErrVectorIntegrity", err)
	}
	if _, err := reader.ValidateCorpus(context.Background()); !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("ValidateCorpus(alternate unit vector) error = %v, want ErrReindexRequired", err)
	}
}

func TestBindActiveRejectsCoherentSemanticMetadataMutations(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*testing.T, *sql.DB, index.Generation)
	}{
		{
			name: "profile and model",
			mutate: func(t *testing.T, database *sql.DB, generation index.Generation) {
				if _, err := database.Exec(`
					UPDATE generations
					SET profile_fingerprint = 'coherent-other-fingerprint', profile_model = 'coherent-other-model'
					WHERE repository_id = ? AND generation_id = ?`, generation.RepositoryID, generation.ID,
				); err != nil {
					t.Fatalf("mutate profile/model error = %v", err)
				}
			},
		},
		{
			name: "dimensions and vector",
			mutate: func(t *testing.T, database *sql.DB, generation index.Generation) {
				blob, err := encodeVector(embedding.Vector{1, 0, 0})
				if err != nil {
					t.Fatalf("encode coherent vector error = %v", err)
				}
				if _, err := database.Exec(`
					UPDATE generations SET dimensions = 3 WHERE repository_id = ? AND generation_id = ?;
					UPDATE vectors SET dimensions = 3, values_blob = ? WHERE repository_id = ? AND generation_id = ?`,
					generation.RepositoryID, generation.ID, blob, generation.RepositoryID, generation.ID,
				); err != nil {
					t.Fatalf("mutate dimensions/vector error = %v", err)
				}
			},
		},
		{
			name: "metric",
			mutate: func(t *testing.T, database *sql.DB, generation index.Generation) {
				if _, err := database.Exec(`
					PRAGMA ignore_check_constraints=ON;
					UPDATE generations SET metric = 'dot-product' WHERE repository_id = ? AND generation_id = ?`,
					generation.RepositoryID, generation.ID,
				); err != nil {
					t.Fatalf("mutate metric error = %v", err)
				}
			},
		},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			directory := t.TempDir()
			store := newVectorStore(t, directory)
			generation := vectorGeneration(t, "repository", "generation", "", []source.Chunk{
				vectorChunk("chunk", "private/manifest.py", 1, source.LanguagePython, "PRIVATE_MANIFEST_SOURCE_CANARY"),
			}, []index.VectorRecord{{ChunkID: "chunk", Values: embedding.Vector{1, 0}}})
			if err := store.Replace(context.Background(), generation); err != nil {
				t.Fatalf("Replace() error = %v", err)
			}
			database, err := sql.Open("sqlite", filepath.Join(directory, databaseName))
			if err != nil {
				t.Fatalf("sql.Open() error = %v", err)
			}
			mutation.mutate(t, database, generation)
			if err := database.Close(); err != nil {
				t.Fatalf("close mutation database error = %v", err)
			}
			reader, err := store.BindActive(context.Background(), generation.RepositoryID)
			if reader != nil {
				_ = reader.Close()
				t.Fatal("BindActive(coherent metadata mutation) returned reader, want nil")
			}
			if !errors.Is(err, index.ErrReindexRequired) {
				t.Fatalf("BindActive(coherent metadata mutation) error = %v, want ErrReindexRequired", err)
			}
		})
	}
}

func TestBindActiveRejectsNonCanonicalGenerationDigests(t *testing.T) {
	for _, test := range []struct {
		name   string
		column string
		value  string
	}{
		{name: "vector uppercase", column: "vector_digest", value: strings.Repeat("A", 64)},
		{name: "manifest short", column: "manifest_digest", value: "abcd"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			store := newVectorStore(t, directory)
			generation := vectorGeneration(t, "repository", "generation", "", []source.Chunk{
				vectorChunk("chunk", "private/noncanonical.py", 1, source.LanguagePython, "PRIVATE_NONCANONICAL_SOURCE_CANARY"),
			}, []index.VectorRecord{{ChunkID: "chunk", Values: embedding.Vector{1, 0}}})
			if err := store.Replace(context.Background(), generation); err != nil {
				t.Fatalf("Replace() error = %v", err)
			}
			database, err := sql.Open("sqlite", filepath.Join(directory, databaseName))
			if err != nil {
				t.Fatalf("sql.Open() error = %v", err)
			}
			if _, err := database.Exec(`UPDATE generations SET `+test.column+` = ? WHERE repository_id = ? AND generation_id = ?`, test.value, generation.RepositoryID, generation.ID); err != nil {
				_ = database.Close()
				t.Fatalf("mutate %s error = %v", test.column, err)
			}
			if err := database.Close(); err != nil {
				t.Fatalf("close mutation database error = %v", err)
			}
			reader, err := store.BindActive(context.Background(), generation.RepositoryID)
			if reader != nil {
				_ = reader.Close()
				t.Fatal("BindActive(noncanonical digest) returned reader, want nil")
			}
			if !errors.Is(err, index.ErrReindexRequired) {
				t.Fatalf("BindActive(noncanonical digest) error = %v, want ErrReindexRequired", err)
			}
		})
	}
}

func TestLexicalOnlyGenerationPersistsCanonicalEmptyVectorDigest(t *testing.T) {
	directory := t.TempDir()
	store := newVectorStore(t, directory)
	generation := lexicalVectorGeneration(t, "repository", "generation", "chunk", "lexical.py")
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	want, err := preparedVectorDigestContext(context.Background(), nil)
	if err != nil {
		t.Fatalf("preparedVectorDigestContext(empty) error = %v", err)
	}
	database, err := sql.Open("sqlite", filepath.Join(directory, databaseName))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer database.Close()
	var got string
	if err := database.QueryRow(`
		SELECT vector_digest FROM generations
		WHERE repository_id = ? AND generation_id = ?`, generation.RepositoryID, generation.ID,
	).Scan(&got); err != nil {
		t.Fatalf("query lexical vector digest error = %v", err)
	}
	if got != want {
		t.Fatalf("lexical vector digest = %q, want canonical empty %q", got, want)
	}
	reader := bindVectorReader(t, store, generation.RepositoryID)
	if _, err := reader.ValidateCorpus(context.Background()); err != nil {
		t.Fatalf("ValidateCorpus(lexical) error = %v", err)
	}
}

func TestExactSearchRanksKnownCosinesAndReturnsCanonicalChunks(t *testing.T) {
	store := newVectorStore(t, t.TempDir())
	chunks := []source.Chunk{
		vectorChunk("negative", "pkg/negative.py", 5, source.LanguagePython, "NEGATIVE_SOURCE_CANARY"),
		vectorChunk("zero", "pkg/zero.py", 3, source.LanguagePython, "ZERO_SOURCE_CANARY"),
		vectorChunk("positive", "pkg/positive.py", 1, source.LanguagePython, "POSITIVE_SOURCE_CANARY"),
	}
	generation := vectorGeneration(t, "repository", "generation", "", chunks, []index.VectorRecord{
		{ChunkID: "negative", Values: embedding.Vector{-1, 0}},
		{ChunkID: "zero", Values: embedding.Vector{0, 8}},
		{ChunkID: "positive", Values: embedding.Vector{3, 0}},
	})
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	reader := bindVectorReader(t, store, generation.RepositoryID)

	metadata, err := reader.Describe(context.Background(), generation.RepositoryID)
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if metadata.GenerationID != generation.ID || metadata.CorpusRevision != generation.CorpusRevision ||
		metadata.Profile != *generation.Profile || metadata.Dimensions != generation.Dimensions || metadata.Metric != generation.Metric {
		t.Fatalf("Describe() = %#v, want pinned generation metadata", metadata)
	}

	candidates, err := reader.Search(context.Background(), vectorIndexQuery(generation, embedding.Vector{9, 0}, 3))
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	wantIDs := []string{"positive", "zero", "negative"}
	wantSimilarities := []float64{1, 0, -1}
	if len(candidates) != len(wantIDs) {
		t.Fatalf("Search() candidates = %#v, want %d", candidates, len(wantIDs))
	}
	chunkByID := make(map[string]source.Chunk, len(chunks))
	for _, chunk := range chunks {
		chunkByID[chunk.ID] = chunk
	}
	for position, candidate := range candidates {
		if candidate.Chunk.ID != wantIDs[position] {
			t.Errorf("candidate %d ID = %q, want %q", position, candidate.Chunk.ID, wantIDs[position])
		}
		if math.Abs(candidate.Similarity-wantSimilarities[position]) > 1e-7 {
			t.Errorf("candidate %d similarity = %v, want raw cosine %v", position, candidate.Similarity, wantSimilarities[position])
		}
		if !reflect.DeepEqual(candidate.Chunk, chunkByID[candidate.Chunk.ID]) {
			t.Errorf("candidate %d chunk = %#v, want canonical %#v", position, candidate.Chunk, chunkByID[candidate.Chunk.ID])
		}
	}
}

func TestHybridLoadThenExactSearchReusesOneValidatedCanonicalChunkScan(t *testing.T) {
	directory := t.TempDir()
	store := newVectorStore(t, directory)
	chunks := []source.Chunk{
		vectorChunk("first", "pkg/first.py", 1, source.LanguagePython, "FIRST_SOURCE"),
		vectorChunk("second", "pkg/second.py", 2, source.LanguagePython, "SECOND_SOURCE"),
	}
	generation := vectorGeneration(t, "repository", "generation", "", chunks, []index.VectorRecord{
		{ChunkID: "first", Values: embedding.Vector{1, 0}},
		{ChunkID: "second", Values: embedding.Vector{0, 1}},
	})
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	database, err := sql.Open("sqlite", filepath.Join(directory, databaseName))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := database.Exec(`
		PRAGMA foreign_keys=OFF;
		ALTER TABLE chunks RENAME TO probed_chunks;
		CREATE VIEW chunks AS
		SELECT repository_id, generation_id, chunk_id, ordinal,
		       gocontext_test_canonical_chunk_read(text) AS text,
		       language, symbol_name, path, start_line, end_line
		FROM probed_chunks;`); err != nil {
		_ = database.Close()
		t.Fatalf("install canonical chunk read probe error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close probe database error = %v", err)
	}

	canonicalChunkReadProbe.Store(0)
	reader := bindVectorReader(t, store, generation.RepositoryID)
	loaded, err := reader.Load(context.Background(), generation.RepositoryID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	loaded[0].Text = "MUTATED_CALLER_COPY"
	candidates, err := reader.Search(context.Background(), vectorIndexQuery(generation, embedding.Vector{1, 0}, 2))
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(candidates) != 2 || candidates[0].Chunk.Text != "FIRST_SOURCE" {
		t.Fatalf("Search() candidates = %#v, want defensive cached canonical chunks", candidates)
	}
	loadedAgain, err := reader.Load(context.Background(), generation.RepositoryID)
	if err != nil {
		t.Fatalf("Load(cached) error = %v", err)
	}
	if loadedAgain[0].Text != "FIRST_SOURCE" {
		t.Fatalf("Load(cached) = %#v, want caller mutation isolated", loadedAgain)
	}
	if got, want := canonicalChunkReadProbe.Load(), int64(len(chunks)); got != want {
		t.Fatalf("canonical chunk rows read = %d, want exactly one %d-row scan across Load+Search", got, want)
	}
}

func TestExactSearchUsesStableReferenceTieBreakers(t *testing.T) {
	store := newVectorStore(t, t.TempDir())
	chunks := []source.Chunk{
		vectorChunk("path-b", "b.py", 1, source.LanguagePython, "PATH_B"),
		vectorChunk("line-late", "a.py", 2, source.LanguagePython, "LINE_LATE"),
		vectorChunk("end-late", "a.py", 1, source.LanguagePython, "END_LATE"),
		vectorChunk("id-b", "a.py", 1, source.LanguagePython, "ID_B"),
		vectorChunk("id-a", "a.py", 1, source.LanguagePython, "ID_A"),
	}
	chunks[2].Reference.EndLine = 3
	chunks[3].Reference.EndLine = 2
	chunks[4].Reference.EndLine = 2
	records := make([]index.VectorRecord, len(chunks))
	for position, chunk := range chunks {
		records[position] = index.VectorRecord{ChunkID: chunk.ID, Values: embedding.Vector{1, 1}}
	}
	generation := vectorGeneration(t, "repository", "ties", "", chunks, records)
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	reader := bindVectorReader(t, store, generation.RepositoryID)

	candidates, err := reader.Search(context.Background(), vectorIndexQuery(generation, embedding.Vector{2, 2}, len(chunks)))
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	want := []string{"id-a", "id-b", "end-late", "line-late", "path-b"}
	for position, candidate := range candidates {
		if candidate.Chunk.ID != want[position] {
			t.Fatalf("candidate order = %#v; position %d ID = %q, want %q", candidates, position, candidate.Chunk.ID, want[position])
		}
	}
}

func TestExactSearchAppliesFilterBeforeExactLimit(t *testing.T) {
	store := newVectorStore(t, t.TempDir())
	chunks := []source.Chunk{
		vectorChunk("excluded-best", "cmd/best.py", 1, source.LanguagePython, "EXCLUDED_BEST"),
		vectorChunk("included-typescript", "internal/app.ts", 2, source.LanguageTypeScript, "INCLUDED_TYPESCRIPT"),
		vectorChunk("included-python", "internal/app.py", 3, source.LanguagePython, "INCLUDED_PYTHON"),
	}
	generation := vectorGeneration(t, "repository", "filters", "", chunks, []index.VectorRecord{
		{ChunkID: "excluded-best", Values: embedding.Vector{1, 0}},
		{ChunkID: "included-typescript", Values: embedding.Vector{0.9, 0.1}},
		{ChunkID: "included-python", Values: embedding.Vector{0.8, 0.2}},
	})
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	reader := bindVectorReader(t, store, generation.RepositoryID)
	query := vectorIndexQuery(generation, embedding.Vector{1, 0}, 1)
	query.Filter = search.Filter{
		PathPrefixes: []string{"internal", "other"},
		Languages:    []source.Language{source.LanguageTypeScript},
	}

	candidates, err := reader.Search(context.Background(), query)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(candidates) != 1 || candidates[0].Chunk.ID != "included-typescript" {
		t.Fatalf("Search() = %#v, want filtered candidate before exact limit", candidates)
	}
}

func TestExactSearchRejectsInvalidFilterBeforeReaderAccess(t *testing.T) {
	store := newVectorStore(t, t.TempDir())
	generation := vectorGeneration(t, "repository", "generation", "", []source.Chunk{
		vectorChunk("chunk", "valid.py", 1, source.LanguagePython, "FILTER_SOURCE_CANARY"),
	}, []index.VectorRecord{{ChunkID: "chunk", Values: embedding.Vector{1, 0}}})
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	reader := bindVectorReader(t, store, generation.RepositoryID)
	if err := reader.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	query := vectorIndexQuery(generation, embedding.Vector{1, 0}, 1)
	query.Filter = search.Filter{PathPrefixes: []string{"../private"}}

	_, err := reader.Search(context.Background(), query)
	if !errors.Is(err, search.ErrInvalidFilter) {
		t.Fatalf("Search(invalid filter on closed reader) error = %v, want ErrInvalidFilter before database access", err)
	}
}

func TestVectorPublicationRejectsIncompleteOrInvalidGenerationAtomically(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*index.Generation)
	}{
		{name: "missing vector", mutate: func(g *index.Generation) { g.Vectors = g.Vectors[:1] }},
		{name: "duplicate chunk ID", mutate: func(g *index.Generation) { g.Vectors[1].ChunkID = g.Vectors[0].ChunkID }},
		{name: "unknown chunk ID", mutate: func(g *index.Generation) { g.Vectors[1].ChunkID = "UNKNOWN_VECTOR_ID_CANARY" }},
		{name: "dimension mismatch", mutate: func(g *index.Generation) { g.Vectors[1].Values = embedding.Vector{1} }},
		{name: "NaN", mutate: func(g *index.Generation) { g.Vectors[1].Values = embedding.Vector{float32(math.NaN()), 1} }},
		{name: "infinity", mutate: func(g *index.Generation) { g.Vectors[1].Values = embedding.Vector{float32(math.Inf(1)), 1} }},
		{name: "zero vector", mutate: func(g *index.Generation) { g.Vectors[1].Values = embedding.Vector{0, 0} }},
		{name: "profile missing", mutate: func(g *index.Generation) { g.Profile = nil }},
		{name: "profile fingerprint missing", mutate: func(g *index.Generation) { g.Profile.Fingerprint = "" }},
		{name: "profile model missing", mutate: func(g *index.Generation) { g.Profile.Model = "" }},
		{name: "dimensions missing", mutate: func(g *index.Generation) { g.Dimensions = 0 }},
		{name: "metric mismatch", mutate: func(g *index.Generation) { g.Metric = index.VectorMetric("PRIVATE_METRIC_CANARY") }},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			store := newVectorStore(t, t.TempDir())
			first := vectorGeneration(t, "repository", "first", "", []source.Chunk{
				vectorChunk("first", "private/first.py", 1, source.LanguagePython, "PRIVATE_FIRST_SOURCE_CANARY"),
			}, []index.VectorRecord{{ChunkID: "first", Values: embedding.Vector{1, 0}}})
			if err := store.Replace(context.Background(), first); err != nil {
				t.Fatalf("Replace(first) error = %v", err)
			}
			second := vectorGeneration(t, "repository", "second", first.ID, []source.Chunk{
				vectorChunk("second-a", "private/second-a.py", 2, source.LanguagePython, "PRIVATE_SECOND_A_SOURCE_CANARY"),
				vectorChunk("second-b", "private/second-b.py", 3, source.LanguagePython, "PRIVATE_SECOND_B_SOURCE_CANARY"),
			}, []index.VectorRecord{
				{ChunkID: "second-a", Values: embedding.Vector{1, 2}},
				{ChunkID: "second-b", Values: embedding.Vector{3, 4}},
			})
			tt.mutate(&second)

			err := store.Replace(context.Background(), second)
			if !errors.Is(err, index.ErrInvalidGeneration) {
				t.Fatalf("Replace(invalid vectors) error = %v, want ErrInvalidGeneration", err)
			}
			for _, canary := range []string{
				"PRIVATE_FIRST_SOURCE_CANARY", "PRIVATE_SECOND_A_SOURCE_CANARY", "PRIVATE_SECOND_B_SOURCE_CANARY",
				"private/", "UNKNOWN_VECTOR_ID_CANARY", "PRIVATE_METRIC_CANARY", "profile-fingerprint", "profile-model",
			} {
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("Replace(invalid vectors) error exposes %q: %v", canary, err)
				}
			}
			active, activeErr := store.ActiveGeneration(context.Background(), first.RepositoryID)
			if activeErr != nil || active != first.ID {
				t.Fatalf("ActiveGeneration() = %q, %v; want preserved %q", active, activeErr, first.ID)
			}
		})
	}
}

func TestVectorPublicationFailureDuringInsertPreservesManifestAndSanitizesError(t *testing.T) {
	directory := t.TempDir()
	store := newVectorStore(t, directory)
	first := vectorGeneration(t, "repository", "first", "", []source.Chunk{
		vectorChunk("first", "private/first.py", 1, source.LanguagePython, "PRIVATE_FIRST_INSERT_SOURCE_CANARY"),
	}, []index.VectorRecord{{ChunkID: "first", Values: embedding.Vector{1, 0}}})
	if err := store.Replace(context.Background(), first); err != nil {
		t.Fatalf("Replace(first) error = %v", err)
	}
	database, err := sql.Open("sqlite", filepath.Join(directory, databaseName))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := database.Exec(`
		CREATE TRIGGER reject_private_vector_insert
		BEFORE INSERT ON vectors
		WHEN NEW.generation_id = 'second'
		BEGIN
			SELECT RAISE(ABORT, 'PRIVATE_TRIGGER_BODY_CANARY');
		END`); err != nil {
		_ = database.Close()
		t.Fatalf("create vector failure trigger error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close trigger database error = %v", err)
	}
	second := vectorGeneration(t, "repository", "second", first.ID, []source.Chunk{
		vectorChunk("second", "private/second.py", 2, source.LanguagePython, "PRIVATE_SECOND_INSERT_SOURCE_CANARY"),
	}, []index.VectorRecord{{ChunkID: "second", Values: embedding.Vector{12345.5, -6789.25}}})

	err = store.Replace(context.Background(), second)
	if err == nil {
		t.Fatal("Replace(second) error = nil, want injected vector insertion failure")
	}
	for _, canary := range []string{
		"PRIVATE_TRIGGER_BODY_CANARY", "PRIVATE_SECOND_INSERT_SOURCE_CANARY", "private/second.py", "12345.5", "6789.25",
	} {
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("Replace(second) error exposes %q: %v", canary, err)
		}
	}
	active, activeErr := store.ActiveGeneration(context.Background(), first.RepositoryID)
	if activeErr != nil || active != first.ID {
		t.Fatalf("ActiveGeneration() = %q, %v; want preserved %q", active, activeErr, first.ID)
	}
	database, err = sql.Open("sqlite", filepath.Join(directory, databaseName))
	if err != nil {
		t.Fatalf("reopen database error = %v", err)
	}
	defer database.Close()
	var secondRows int
	if err := database.QueryRow(`SELECT count(*) FROM generations WHERE generation_id = 'second'`).Scan(&secondRows); err != nil {
		t.Fatalf("count rolled-back vector generation error = %v", err)
	}
	if secondRows != 0 {
		t.Fatalf("rolled-back vector generation rows = %d, want 0", secondRows)
	}
}

func TestExactSearchRejectsIncompatibleSpaceBeforeScanning(t *testing.T) {
	store := newVectorStore(t, t.TempDir())
	generation := vectorGeneration(t, "repository", "generation", "", []source.Chunk{
		vectorChunk("chunk", "private/incompatible.py", 1, source.LanguagePython, "PRIVATE_INCOMPATIBLE_SOURCE_CANARY"),
	}, []index.VectorRecord{{ChunkID: "chunk", Values: embedding.Vector{1, 0}}})
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	reader := bindVectorReader(t, store, generation.RepositoryID)

	queries := []struct {
		name   string
		mutate func(*vectorsearch.IndexQuery)
		want   error
	}{
		{name: "repository", mutate: func(q *vectorsearch.IndexQuery) { q.RepositoryID = "other-repository" }, want: vectorsearch.ErrIncompatibleSpace},
		{name: "generation", mutate: func(q *vectorsearch.IndexQuery) { q.GenerationID = "other-generation" }, want: vectorsearch.ErrGenerationChanged},
		{name: "fingerprint", mutate: func(q *vectorsearch.IndexQuery) { q.Profile.Fingerprint = "PRIVATE_FINGERPRINT_CANARY" }, want: vectorsearch.ErrIncompatibleSpace},
		{name: "model", mutate: func(q *vectorsearch.IndexQuery) { q.Profile.Model = "PRIVATE_MODEL_CANARY" }, want: vectorsearch.ErrIncompatibleSpace},
		{name: "dimensions", mutate: func(q *vectorsearch.IndexQuery) { q.Dimensions = 3; q.Vector = embedding.Vector{1, 0, 0} }, want: vectorsearch.ErrIncompatibleSpace},
		{name: "metric", mutate: func(q *vectorsearch.IndexQuery) { q.Metric = index.VectorMetric("PRIVATE_QUERY_METRIC_CANARY") }, want: vectorsearch.ErrIncompatibleSpace},
	}
	for _, tt := range queries {
		t.Run(tt.name, func(t *testing.T) {
			query := vectorIndexQuery(generation, embedding.Vector{1, 0}, 1)
			tt.mutate(&query)
			_, err := reader.Search(context.Background(), query)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Search(incompatible space) error = %v, want %v", err, tt.want)
			}
			for _, canary := range []string{"PRIVATE_INCOMPATIBLE_SOURCE_CANARY", "private/incompatible.py", "PRIVATE_FINGERPRINT_CANARY", "PRIVATE_MODEL_CANARY", "PRIVATE_QUERY_METRIC_CANARY"} {
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("Search(incompatible space) error exposes %q: %v", canary, err)
				}
			}
		})
	}
	if _, err := reader.Describe(context.Background(), "other-repository"); !errors.Is(err, vectorsearch.ErrIncompatibleSpace) {
		t.Fatalf("Describe(other repository) error = %v, want ErrIncompatibleSpace", err)
	}
}

func TestExactSearchReportsGenerationChangedSeparatelyFromSpaceMismatch(t *testing.T) {
	store := newVectorStore(t, t.TempDir())
	generation := vectorGeneration(t, "repository", "generation", "", []source.Chunk{
		vectorChunk("chunk", "generation.py", 1, source.LanguagePython, "GENERATION_SOURCE"),
	}, []index.VectorRecord{{ChunkID: "chunk", Values: embedding.Vector{1, 0}}})
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	reader := bindVectorReader(t, store, generation.RepositoryID)
	query := vectorIndexQuery(generation, embedding.Vector{1, 0}, 1)
	query.GenerationID = "other-generation"

	_, err := reader.Search(context.Background(), query)
	if !errors.Is(err, vectorsearch.ErrGenerationChanged) {
		t.Fatalf("Search(generation mismatch) error = %v, want ErrGenerationChanged", err)
	}
	if errors.Is(err, vectorsearch.ErrIncompatibleSpace) {
		t.Fatalf("Search(generation mismatch) error = %v, must not conflate space incompatibility", err)
	}
}

func TestExactSearchGenerationChangePrecedesMalformedQueryValidation(t *testing.T) {
	store := newVectorStore(t, t.TempDir())
	generation := vectorGeneration(t, "repository", "generation", "", []source.Chunk{
		vectorChunk("chunk", "generation-preflight.py", 1, source.LanguagePython, "GENERATION_PREFLIGHT_SOURCE"),
	}, []index.VectorRecord{{ChunkID: "chunk", Values: embedding.Vector{1, 0}}})
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	reader := bindVectorReader(t, store, generation.RepositoryID)

	staleQueries := []struct {
		name   string
		mutate func(*vectorsearch.IndexQuery)
		lower  error
	}{
		{
			name: "invalid filter",
			mutate: func(query *vectorsearch.IndexQuery) {
				query.Filter = search.Filter{PathPrefixes: []string{"../private"}}
			},
			lower: search.ErrInvalidFilter,
		},
		{
			name: "malformed vector",
			mutate: func(query *vectorsearch.IndexQuery) {
				query.Vector = nil
			},
			lower: vectorsearch.ErrInvalidQueryVector,
		},
	}
	for _, test := range staleQueries {
		t.Run("stale generation with "+test.name, func(t *testing.T) {
			query := vectorIndexQuery(generation, embedding.Vector{1, 0}, 1)
			query.GenerationID = "stale-generation"
			test.mutate(&query)

			_, err := reader.Search(context.Background(), query)
			if !errors.Is(err, vectorsearch.ErrGenerationChanged) {
				t.Fatalf("Search() error = %v, want ErrGenerationChanged", err)
			}
			if errors.Is(err, test.lower) {
				t.Fatalf("Search() error = %v, stale generation must precede %v", err, test.lower)
			}
		})
	}

	if err := reader.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	for _, test := range staleQueries {
		t.Run("same generation "+test.name+" before closed reader access", func(t *testing.T) {
			query := vectorIndexQuery(generation, embedding.Vector{1, 0}, 1)
			test.mutate(&query)

			_, err := reader.Search(context.Background(), query)
			if !errors.Is(err, test.lower) {
				t.Fatalf("Search() error = %v, want %v before closed reader/database access", err, test.lower)
			}
		})
	}
}

func TestExactSearchRejectsInvalidQueryVectorsAndLimits(t *testing.T) {
	store := newVectorStore(t, t.TempDir())
	generation := vectorGeneration(t, "repository", "generation", "", []source.Chunk{
		vectorChunk("chunk", "query.py", 1, source.LanguagePython, "QUERY_SOURCE"),
	}, []index.VectorRecord{{ChunkID: "chunk", Values: embedding.Vector{1, 0}}})
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	reader := bindVectorReader(t, store, generation.RepositoryID)

	queries := []struct {
		name   string
		values embedding.Vector
		limit  int
	}{
		{name: "empty", values: nil, limit: 1},
		{name: "dimension mismatch", values: embedding.Vector{1}, limit: 1},
		{name: "NaN", values: embedding.Vector{float32(math.NaN()), 1}, limit: 1},
		{name: "infinity", values: embedding.Vector{float32(math.Inf(-1)), 1}, limit: 1},
		{name: "zero", values: embedding.Vector{0, 0}, limit: 1},
		{name: "zero limit", values: embedding.Vector{1, 0}, limit: 0},
		{name: "negative limit", values: embedding.Vector{1, 0}, limit: -1},
		{name: "overflow-prone limit", values: embedding.Vector{1, 0}, limit: int(^uint(0) >> 1)},
	}
	for _, tt := range queries {
		t.Run(tt.name, func(t *testing.T) {
			query := vectorIndexQuery(generation, tt.values, tt.limit)
			_, err := reader.Search(context.Background(), query)
			if !errors.Is(err, vectorsearch.ErrInvalidQueryVector) {
				t.Fatalf("Search(invalid query) error = %v, want ErrInvalidQueryVector", err)
			}
		})
	}
}

func TestExactSearchReportsLexicalOnlyGenerationUnavailable(t *testing.T) {
	store := newVectorStore(t, t.TempDir())
	chunk := vectorChunk("chunk", "lexical.py", 1, source.LanguagePython, "LEXICAL_ONLY_SOURCE_CANARY")
	corpus, err := source.NewCorpus(ingest.ScanPolicyVersion, []source.Chunk{chunk})
	if err != nil {
		t.Fatalf("NewCorpus() error = %v", err)
	}
	generation := index.Generation{
		RepositoryID:      "repository",
		ID:                "lexical-only",
		CorpusRevision:    corpus.Revision,
		ScanPolicyVersion: corpus.PolicyVersion,
		Chunks:            corpus.Chunks,
		Metric:            index.VectorMetricCosine,
	}
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	reader := bindVectorReader(t, store, generation.RepositoryID)
	if _, err := reader.Describe(context.Background(), generation.RepositoryID); !errors.Is(err, vectorsearch.ErrVectorUnavailable) {
		t.Fatalf("Describe(lexical only) error = %v, want ErrVectorUnavailable", err)
	}
	query := vectorsearch.IndexQuery{
		RepositoryID: generation.RepositoryID,
		GenerationID: generation.ID,
		Profile:      embedding.Profile{Fingerprint: "expected", Model: "expected"},
		Dimensions:   2,
		Metric:       index.VectorMetricCosine,
		Vector:       embedding.Vector{1, 0},
		Limit:        1,
	}
	if _, err := reader.Search(context.Background(), query); !errors.Is(err, vectorsearch.ErrVectorUnavailable) {
		t.Fatalf("Search(lexical only) error = %v, want ErrVectorUnavailable", err)
	}
}

func TestExactSearchLexicalOnlyGenerationRejectsUnexpectedLinkedVectorRows(t *testing.T) {
	store := newVectorStore(t, t.TempDir())
	generation := lexicalVectorGeneration(t, "repository", "lexical-corrupt", "chunk", "private/lexical-corrupt.py")
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	blob, err := encodeVector(embedding.Vector{1, 0})
	if err != nil {
		t.Fatalf("encodeVector() error = %v", err)
	}
	if _, err := store.db.Exec(`
		INSERT INTO vectors(repository_id, generation_id, chunk_id, encoding_version, dimensions, values_blob)
		VALUES (?, ?, ?, ?, ?, ?)`,
		generation.RepositoryID, generation.ID, generation.Chunks[0].ID, vectorEncodingVersion, 2, blob,
	); err != nil {
		t.Fatalf("insert unexpected linked vector error = %v", err)
	}
	reader := bindVectorReader(t, store, generation.RepositoryID)

	if _, err := reader.Describe(context.Background(), generation.RepositoryID); !errors.Is(err, vectorsearch.ErrVectorIntegrity) {
		t.Fatalf("Describe(lexical-only corruption) error = %v, want ErrVectorIntegrity", err)
	}
	if _, err := reader.Search(context.Background(), lexicalVectorQuery(generation)); !errors.Is(err, vectorsearch.ErrVectorIntegrity) {
		t.Fatalf("Search(lexical-only corruption) error = %v, want ErrVectorIntegrity", err)
	}
}

func TestExactSearchLexicalOnlyIntegrityCheckIsRepositoryScoped(t *testing.T) {
	store := newVectorStore(t, t.TempDir())
	isolated := lexicalVectorGeneration(t, "isolated-repository", "shared-generation", "isolated-chunk", "isolated.py")
	other := lexicalVectorGeneration(t, "other-repository", "shared-generation", "other-chunk", "other.py")
	if err := store.Replace(context.Background(), isolated); err != nil {
		t.Fatalf("Replace(isolated) error = %v", err)
	}
	if err := store.Replace(context.Background(), other); err != nil {
		t.Fatalf("Replace(other) error = %v", err)
	}
	blob, err := encodeVector(embedding.Vector{1, 0})
	if err != nil {
		t.Fatalf("encodeVector() error = %v", err)
	}
	if _, err := store.db.Exec(`
		INSERT INTO vectors(repository_id, generation_id, chunk_id, encoding_version, dimensions, values_blob)
		VALUES (?, ?, ?, ?, ?, ?)`,
		other.RepositoryID, other.ID, other.Chunks[0].ID, vectorEncodingVersion, 2, blob,
	); err != nil {
		t.Fatalf("insert other repository vector error = %v", err)
	}
	reader := bindVectorReader(t, store, isolated.RepositoryID)

	if _, err := reader.Describe(context.Background(), isolated.RepositoryID); !errors.Is(err, vectorsearch.ErrVectorUnavailable) {
		t.Fatalf("Describe(isolated lexical-only) error = %v, want ErrVectorUnavailable", err)
	}
	if _, err := reader.Search(context.Background(), lexicalVectorQuery(isolated)); !errors.Is(err, vectorsearch.ErrVectorUnavailable) {
		t.Fatalf("Search(isolated lexical-only) error = %v, want ErrVectorUnavailable", err)
	}
}

func TestExactSearchRejectsMissingMalformedDuplicateAndOrphanedRows(t *testing.T) {
	validBlob, err := encodeVector(embedding.Vector{1, 0})
	if err != nil {
		t.Fatalf("encodeVector(valid corruption fixture) error = %v", err)
	}
	nanBlob := make([]byte, 8)
	binary.LittleEndian.PutUint32(nanBlob, math.Float32bits(float32(math.NaN())))
	mutations := []struct {
		name   string
		mutate func(*testing.T, *sql.DB, index.Generation)
	}{
		{
			name: "missing row",
			mutate: func(t *testing.T, database *sql.DB, generation index.Generation) {
				if _, err := database.Exec(`DELETE FROM vectors WHERE repository_id = ? AND generation_id = ?`, generation.RepositoryID, generation.ID); err != nil {
					t.Fatalf("delete vector error = %v", err)
				}
			},
		},
		{
			name: "partial blob",
			mutate: func(t *testing.T, database *sql.DB, generation index.Generation) {
				if _, err := database.Exec(`UPDATE vectors SET values_blob = X'010203' WHERE repository_id = ? AND generation_id = ?`, generation.RepositoryID, generation.ID); err != nil {
					t.Fatalf("corrupt vector blob error = %v", err)
				}
			},
		},
		{
			name: "dimension mismatch",
			mutate: func(t *testing.T, database *sql.DB, generation index.Generation) {
				if _, err := database.Exec(`UPDATE vectors SET dimensions = 3 WHERE repository_id = ? AND generation_id = ?`, generation.RepositoryID, generation.ID); err != nil {
					t.Fatalf("corrupt vector dimensions error = %v", err)
				}
			},
		},
		{
			name: "negative dimensions",
			mutate: func(t *testing.T, database *sql.DB, generation index.Generation) {
				if _, err := database.Exec(`UPDATE vectors SET dimensions = ? WHERE repository_id = ? AND generation_id = ?`, int64(-1), generation.RepositoryID, generation.ID); err != nil {
					t.Fatalf("set negative vector dimensions error = %v", err)
				}
			},
		},
		{
			name: "maximum dimensions",
			mutate: func(t *testing.T, database *sql.DB, generation index.Generation) {
				if _, err := database.Exec(`UPDATE vectors SET dimensions = ? WHERE repository_id = ? AND generation_id = ?`, int64(math.MaxInt64), generation.RepositoryID, generation.ID); err != nil {
					t.Fatalf("set maximum vector dimensions error = %v", err)
				}
			},
		},
		{
			name: "32-bit wrapping dimensions",
			mutate: func(t *testing.T, database *sql.DB, generation index.Generation) {
				wrappingDimensions := (int64(1) << 32) + int64(generation.Dimensions)
				if _, err := database.Exec(`UPDATE vectors SET dimensions = ? WHERE repository_id = ? AND generation_id = ?`, wrappingDimensions, generation.RepositoryID, generation.ID); err != nil {
					t.Fatalf("set wrapping vector dimensions error = %v", err)
				}
			},
		},
		{
			name: "unknown encoding",
			mutate: func(t *testing.T, database *sql.DB, generation index.Generation) {
				if _, err := database.Exec(`UPDATE vectors SET encoding_version = 2 WHERE repository_id = ? AND generation_id = ?`, generation.RepositoryID, generation.ID); err != nil {
					t.Fatalf("corrupt vector encoding error = %v", err)
				}
			},
		},
		{
			name: "negative encoding",
			mutate: func(t *testing.T, database *sql.DB, generation index.Generation) {
				if _, err := database.Exec(`UPDATE vectors SET encoding_version = ? WHERE repository_id = ? AND generation_id = ?`, int64(-1), generation.RepositoryID, generation.ID); err != nil {
					t.Fatalf("set negative vector encoding error = %v", err)
				}
			},
		},
		{
			name: "maximum encoding",
			mutate: func(t *testing.T, database *sql.DB, generation index.Generation) {
				if _, err := database.Exec(`UPDATE vectors SET encoding_version = ? WHERE repository_id = ? AND generation_id = ?`, int64(math.MaxInt64), generation.RepositoryID, generation.ID); err != nil {
					t.Fatalf("set maximum vector encoding error = %v", err)
				}
			},
		},
		{
			name: "32-bit wrapping encoding",
			mutate: func(t *testing.T, database *sql.DB, generation index.Generation) {
				wrappingVersion := (int64(1) << 32) + int64(vectorEncodingVersion)
				if _, err := database.Exec(`UPDATE vectors SET encoding_version = ? WHERE repository_id = ? AND generation_id = ?`, wrappingVersion, generation.RepositoryID, generation.ID); err != nil {
					t.Fatalf("set wrapping vector encoding error = %v", err)
				}
			},
		},
		{
			name: "NaN component",
			mutate: func(t *testing.T, database *sql.DB, generation index.Generation) {
				if _, err := database.Exec(`UPDATE vectors SET values_blob = ? WHERE repository_id = ? AND generation_id = ?`, nanBlob, generation.RepositoryID, generation.ID); err != nil {
					t.Fatalf("corrupt vector component error = %v", err)
				}
			},
		},
		{
			name: "non-unit stored vector",
			mutate: func(t *testing.T, database *sql.DB, generation index.Generation) {
				nonUnit, err := encodeVector(embedding.Vector{2, 0})
				if err != nil {
					t.Fatalf("encode non-unit vector error = %v", err)
				}
				if _, err := database.Exec(`UPDATE vectors SET values_blob = ? WHERE repository_id = ? AND generation_id = ?`, nonUnit, generation.RepositoryID, generation.ID); err != nil {
					t.Fatalf("corrupt vector normalization error = %v", err)
				}
			},
		},
		{
			name: "orphan row",
			mutate: func(t *testing.T, database *sql.DB, generation index.Generation) {
				if _, err := database.Exec(`
					INSERT INTO vectors(repository_id, generation_id, chunk_id, encoding_version, dimensions, values_blob)
					VALUES (?, ?, 'PRIVATE_ORPHAN_CHUNK_CANARY', ?, 2, ?)`,
					generation.RepositoryID, generation.ID, vectorEncodingVersion, validBlob,
				); err != nil {
					t.Fatalf("insert orphan vector error = %v", err)
				}
			},
		},
		{
			name: "duplicate row",
			mutate: func(t *testing.T, database *sql.DB, _ index.Generation) {
				if _, err := database.Exec(`
					PRAGMA foreign_keys=OFF;
					ALTER TABLE vectors RENAME TO original_vectors;
					CREATE TABLE vectors (
						repository_id TEXT NOT NULL,
						generation_id TEXT NOT NULL,
						chunk_id TEXT NOT NULL,
						encoding_version INTEGER NOT NULL,
						dimensions INTEGER NOT NULL,
						values_blob BLOB NOT NULL
					);
					INSERT INTO vectors SELECT * FROM original_vectors;
					INSERT INTO vectors SELECT * FROM original_vectors;
					DROP TABLE original_vectors;`); err != nil {
					t.Fatalf("duplicate vector row error = %v", err)
				}
			},
		},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			store := newVectorStore(t, directory)
			generation := vectorGeneration(t, "repository", "generation", "", []source.Chunk{
				vectorChunk("chunk", "private/corrupt.py", 1, source.LanguagePython, "PRIVATE_CORRUPT_SOURCE_CANARY"),
			}, []index.VectorRecord{{ChunkID: "chunk", Values: embedding.Vector{1, 0}}})
			if err := store.Replace(context.Background(), generation); err != nil {
				t.Fatalf("Replace() error = %v", err)
			}
			database, err := sql.Open("sqlite", filepath.Join(directory, databaseName))
			if err != nil {
				t.Fatalf("sql.Open() error = %v", err)
			}
			tt.mutate(t, database, generation)
			if err := database.Close(); err != nil {
				t.Fatalf("close corruption database error = %v", err)
			}
			reader := bindVectorReader(t, store, generation.RepositoryID)

			_, err = reader.Search(context.Background(), vectorIndexQuery(generation, embedding.Vector{12345.5, -6789.25}, 1))
			if !errors.Is(err, vectorsearch.ErrVectorIntegrity) {
				t.Fatalf("Search(corrupt vector) error = %v, want ErrVectorIntegrity", err)
			}
			for _, canary := range []string{
				"PRIVATE_CORRUPT_SOURCE_CANARY", "private/corrupt.py", "PRIVATE_ORPHAN_CHUNK_CANARY",
				"12345.5", "6789.25", "profile-fingerprint", "profile-model",
			} {
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("Search(corrupt vector) error exposes %q: %v", canary, err)
				}
			}
		})
	}
}

func TestExactSearchKeepsRepositoriesIsolated(t *testing.T) {
	store := newVectorStore(t, t.TempDir())
	first := vectorGeneration(t, "repository-one", "shared-generation", "", []source.Chunk{
		vectorChunk("shared-chunk", "one.py", 1, source.LanguagePython, "REPOSITORY_ONE_SOURCE"),
	}, []index.VectorRecord{{ChunkID: "shared-chunk", Values: embedding.Vector{1, 0}}})
	second := vectorGeneration(t, "repository-two", "shared-generation", "", []source.Chunk{
		vectorChunk("shared-chunk", "two.py", 1, source.LanguagePython, "REPOSITORY_TWO_SOURCE"),
	}, []index.VectorRecord{{ChunkID: "shared-chunk", Values: embedding.Vector{-1, 0}}})
	for _, generation := range []index.Generation{first, second} {
		if err := store.Replace(context.Background(), generation); err != nil {
			t.Fatalf("Replace(%s) error = %v", generation.RepositoryID, err)
		}
	}
	reader := bindVectorReader(t, store, first.RepositoryID)
	candidates, err := reader.Search(context.Background(), vectorIndexQuery(first, embedding.Vector{1, 0}, 1))
	if err != nil {
		t.Fatalf("Search(repository one) error = %v", err)
	}
	if len(candidates) != 1 || candidates[0].Chunk.Text != "REPOSITORY_ONE_SOURCE" || candidates[0].Similarity != 1 {
		t.Fatalf("Search(repository one) = %#v, want isolated repository-one candidate", candidates)
	}
	query := vectorIndexQuery(second, embedding.Vector{1, 0}, 1)
	if _, err := reader.Search(context.Background(), query); !errors.Is(err, vectorsearch.ErrIncompatibleSpace) {
		t.Fatalf("Search(other repository through bound reader) error = %v, want ErrIncompatibleSpace", err)
	}
}

func TestVectorReaderPinsGenerationAcrossPublicationAndCleanupRetry(t *testing.T) {
	directory := t.TempDir()
	store := newVectorStore(t, directory)
	first := vectorGeneration(t, "repository", "generation-a", "", []source.Chunk{
		vectorChunk("old", "private/old.py", 1, source.LanguagePython, "RETIRED_VECTOR_SOURCE_CANARY_91D7"),
	}, []index.VectorRecord{{ChunkID: "old", Values: embedding.Vector{1, 0}}})
	if err := store.Replace(context.Background(), first); err != nil {
		t.Fatalf("Replace(first) error = %v", err)
	}
	reader := bindVectorReader(t, store, first.RepositoryID)
	if _, err := reader.Search(context.Background(), vectorIndexQuery(first, embedding.Vector{1, 0}, 1)); err != nil {
		t.Fatalf("Search(first before publication) error = %v", err)
	}
	second := vectorGeneration(t, first.RepositoryID, "generation-b", first.ID, []source.Chunk{
		vectorChunk("new", "current/new.py", 1, source.LanguagePython, "CURRENT_VECTOR_SOURCE"),
	}, []index.VectorRecord{{ChunkID: "new", Values: embedding.Vector{0, 1}}})

	started := time.Now()
	err := store.Replace(context.Background(), second)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Replace(second with pinned reader) took %v, want coordinated checkpoint outcome under 1s", elapsed)
	}
	var committed *index.CommittedCleanupError
	if !errors.As(err, &committed) || committed.Stage() != index.CleanupStageCheckpoint || !committed.Published() {
		t.Fatalf("Replace(second) error = %T %v, want published checkpoint cleanup outcome", err, err)
	}

	loaded, err := reader.Load(context.Background(), first.RepositoryID)
	if err != nil || !reflect.DeepEqual(loaded, first.Chunks) {
		t.Fatalf("reader A Load() = %#v, %v; want pinned %#v", loaded, err, first.Chunks)
	}
	candidates, err := reader.Search(context.Background(), vectorIndexQuery(first, embedding.Vector{1, 0}, 1))
	if err != nil || len(candidates) != 1 || candidates[0].Chunk.ID != "old" || candidates[0].Similarity != 1 {
		t.Fatalf("reader A Search() = %#v, %v; want pinned old vector", candidates, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close(reader A) error = %v", err)
	}
	if err := store.Replace(context.Background(), second); err != nil {
		t.Fatalf("Replace(cleanup retry) error = %v", err)
	}

	databasePath := filepath.Join(directory, databaseName)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	var generations int
	if err := database.QueryRow(`SELECT count(*) FROM generations WHERE repository_id = ?`, second.RepositoryID).Scan(&generations); err != nil {
		_ = database.Close()
		t.Fatalf("count generations error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close inspection database error = %v", err)
	}
	if generations != 1 {
		t.Fatalf("stored generations = %d, want only active generation", generations)
	}
	canary := []byte("RETIRED_VECTOR_SOURCE_CANARY_91D7")
	for _, path := range []string{databasePath, databasePath + "-wal"} {
		payload, err := osReadFileAllowMissing(path)
		if err != nil {
			t.Fatalf("read %s error = %v", filepath.Base(path), err)
		}
		if bytes.Contains(payload, canary) {
			t.Fatalf("%s retains retired vector generation canary", filepath.Base(path))
		}
	}
}

func TestVectorPublicationCancellationDuringInsertPreservesManifest(t *testing.T) {
	directory := t.TempDir()
	store := newVectorStore(t, directory)
	first := vectorGeneration(t, "repository", "first", "", []source.Chunk{
		vectorChunk("first", "first.py", 1, source.LanguagePython, "FIRST_CANCELLATION_SOURCE"),
	}, []index.VectorRecord{{ChunkID: "first", Values: embedding.Vector{1, 0}}})
	if err := store.Replace(context.Background(), first); err != nil {
		t.Fatalf("Replace(first) error = %v", err)
	}
	database, err := sql.Open("sqlite", filepath.Join(directory, databaseName))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := database.Exec(`
		CREATE TRIGGER slow_vector_insert
		BEFORE INSERT ON vectors
		WHEN NEW.generation_id = 'second'
		BEGIN
			SELECT sum(value) FROM (
				WITH RECURSIVE counter(value) AS (
					VALUES(0)
					UNION ALL
					SELECT value + 1 FROM counter WHERE value < 100000000
				)
				SELECT value FROM counter
			);
		END`); err != nil {
		_ = database.Close()
		t.Fatalf("create slow vector trigger error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close trigger database error = %v", err)
	}
	second := vectorGeneration(t, "repository", "second", first.ID, []source.Chunk{
		vectorChunk("second", "private/canceled.py", 2, source.LanguagePython, "PRIVATE_CANCELED_SOURCE_CANARY"),
	}, []index.VectorRecord{{ChunkID: "second", Values: embedding.Vector{12345.5, -6789.25}}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	started := time.Now()
	err = store.Replace(ctx, second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Replace(canceled vector insert) error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Replace(canceled vector insert) took %v, want prompt interruption", elapsed)
	}
	for _, canary := range []string{"PRIVATE_CANCELED_SOURCE_CANARY", "private/canceled.py", "12345.5", "6789.25"} {
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("Replace(canceled vector insert) error exposes %q: %v", canary, err)
		}
	}
	active, activeErr := store.ActiveGeneration(context.Background(), first.RepositoryID)
	if activeErr != nil || active != first.ID {
		t.Fatalf("ActiveGeneration() = %q, %v; want preserved %q", active, activeErr, first.ID)
	}
	database, err = sql.Open("sqlite", filepath.Join(directory, databaseName))
	if err != nil {
		t.Fatalf("reopen database error = %v", err)
	}
	defer database.Close()
	var secondRows int
	if err := database.QueryRow(`SELECT count(*) FROM generations WHERE generation_id = 'second'`).Scan(&secondRows); err != nil {
		t.Fatalf("count canceled generation error = %v", err)
	}
	if secondRows != 0 {
		t.Fatalf("canceled vector generation rows = %d, want 0", secondRows)
	}
}

func TestVectorReaderConcurrentSearchLoadCloseAndReplace(t *testing.T) {
	store := newVectorStore(t, t.TempDir())
	first := vectorGeneration(t, "repository", "first", "", []source.Chunk{
		vectorChunk("first", "first.py", 1, source.LanguagePython, "FIRST_CONCURRENT_SOURCE"),
	}, []index.VectorRecord{{ChunkID: "first", Values: embedding.Vector{1, 0}}})
	if err := store.Replace(context.Background(), first); err != nil {
		t.Fatalf("Replace(first) error = %v", err)
	}
	reader, err := store.BindActive(context.Background(), first.RepositoryID)
	if err != nil {
		t.Fatalf("BindActive() error = %v", err)
	}
	second := vectorGeneration(t, first.RepositoryID, "second", first.ID, []source.Chunk{
		vectorChunk("second", "second.py", 1, source.LanguagePython, "SECOND_CONCURRENT_SOURCE"),
	}, []index.VectorRecord{{ChunkID: "second", Values: embedding.Vector{0, 1}}})
	start := make(chan struct{})
	results := make(chan error, 19)
	for iteration := 0; iteration < 8; iteration++ {
		go func() {
			<-start
			_, err := reader.Search(context.Background(), vectorIndexQuery(first, embedding.Vector{1, 0}, 1))
			results <- err
		}()
		go func() {
			<-start
			_, err := reader.Load(context.Background(), first.RepositoryID)
			results <- err
		}()
	}
	go func() {
		<-start
		results <- reader.Close()
	}()
	go func() {
		<-start
		results <- reader.Close()
	}()
	go func() {
		<-start
		results <- store.Replace(context.Background(), second)
	}()
	close(start)

	for count := 0; count < cap(results); count++ {
		err := <-results
		if err == nil || errors.Is(err, errBoundReaderClosed) || errors.Is(err, index.ErrCleanupIncomplete) {
			continue
		}
		t.Fatalf("concurrent operation error = %v", err)
	}
	active, err := store.ActiveGeneration(context.Background(), first.RepositoryID)
	if err != nil || active != second.ID {
		t.Fatalf("ActiveGeneration() = %q, %v; want %q", active, err, second.ID)
	}
	if err := store.Replace(context.Background(), second); err != nil {
		t.Fatalf("Replace(cleanup retry) error = %v", err)
	}
	if _, err := reader.Search(context.Background(), vectorIndexQuery(first, embedding.Vector{1, 0}, 1)); !errors.Is(err, errBoundReaderClosed) {
		t.Fatalf("Search(after close) error = %v, want safe closed-reader error", err)
	}
	if _, err := reader.Load(context.Background(), first.RepositoryID); !errors.Is(err, errBoundReaderClosed) {
		t.Fatalf("Load(after close) error = %v, want safe closed-reader error", err)
	}
	if _, err := reader.Describe(context.Background(), first.RepositoryID); !errors.Is(err, errBoundReaderClosed) {
		t.Fatalf("Describe(after close) error = %v, want safe closed-reader error", err)
	}
}

func TestVectorStoreCloseClosesRegisteredReaderWithoutDeadlock(t *testing.T) {
	store := newVectorStore(t, t.TempDir())
	generation := vectorGeneration(t, "repository", "generation", "", []source.Chunk{
		vectorChunk("chunk", "shutdown.py", 1, source.LanguagePython, "SHUTDOWN_SOURCE"),
	}, []index.VectorRecord{{ChunkID: "chunk", Values: embedding.Vector{1, 0}}})
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	reader, err := store.BindActive(context.Background(), generation.RepositoryID)
	if err != nil {
		t.Fatalf("BindActive() error = %v", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- store.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Store.Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Store.Close() deadlocked with registered reader")
	}
	if _, err := reader.Search(context.Background(), vectorIndexQuery(generation, embedding.Vector{1, 0}, 1)); !errors.Is(err, errBoundReaderClosed) {
		t.Fatalf("Search(after store close) error = %v, want safe closed-reader error", err)
	}
}

func TestVectorIdempotentRetrySanitizesMalformedStoredRow(t *testing.T) {
	directory := t.TempDir()
	store := newVectorStore(t, directory)
	generation := vectorGeneration(t, "repository", "generation", "", []source.Chunk{
		vectorChunk("chunk", "private/idempotent.py", 1, source.LanguagePython, "PRIVATE_IDEMPOTENT_SOURCE_CANARY"),
	}, []index.VectorRecord{{ChunkID: "chunk", Values: embedding.Vector{1, 0}}})
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	database, err := sql.Open("sqlite", filepath.Join(directory, databaseName))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := database.Exec(`UPDATE vectors SET dimensions = 'PRIVATE_STORED_DIMENSION_CANARY'`); err != nil {
		_ = database.Close()
		t.Fatalf("corrupt stored vector dimension error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close corruption database error = %v", err)
	}

	err = store.Replace(context.Background(), generation)
	if !errors.Is(err, index.ErrInvalidGeneration) {
		t.Fatalf("Replace(idempotent corrupted generation) error = %v, want ErrInvalidGeneration", err)
	}
	for _, canary := range []string{"PRIVATE_STORED_DIMENSION_CANARY", "PRIVATE_IDEMPOTENT_SOURCE_CANARY", "private/idempotent.py"} {
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("Replace(idempotent corrupted generation) error exposes %q: %v", canary, err)
		}
	}
}

func TestVectorBindSanitizesMalformedStoredMetadata(t *testing.T) {
	directory := t.TempDir()
	store := newVectorStore(t, directory)
	generation := vectorGeneration(t, "repository", "generation", "", []source.Chunk{
		vectorChunk("chunk", "private/metadata.py", 1, source.LanguagePython, "PRIVATE_METADATA_SOURCE_CANARY"),
	}, []index.VectorRecord{{ChunkID: "chunk", Values: embedding.Vector{1, 0}}})
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	database, err := sql.Open("sqlite", filepath.Join(directory, databaseName))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := database.Exec(`UPDATE generations SET dimensions = 'PRIVATE_STORED_METADATA_CANARY'`); err != nil {
		_ = database.Close()
		t.Fatalf("corrupt generation metadata error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close corruption database error = %v", err)
	}

	_, err = store.BindActive(context.Background(), generation.RepositoryID)
	if !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("BindActive(corrupt metadata) error = %v, want ErrReindexRequired", err)
	}
	for _, canary := range []string{"PRIVATE_STORED_METADATA_CANARY", "PRIVATE_METADATA_SOURCE_CANARY", "private/metadata.py"} {
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("BindActive(corrupt metadata) error exposes %q: %v", canary, err)
		}
	}
}

func TestVectorPublicationSanitizesFailureBeforeVectorRows(t *testing.T) {
	directory := t.TempDir()
	store := newVectorStore(t, directory)
	first := vectorGeneration(t, "repository", "first", "", []source.Chunk{
		vectorChunk("first", "first.py", 1, source.LanguagePython, "FIRST_PREVECTOR_SOURCE"),
	}, []index.VectorRecord{{ChunkID: "first", Values: embedding.Vector{1, 0}}})
	if err := store.Replace(context.Background(), first); err != nil {
		t.Fatalf("Replace(first) error = %v", err)
	}
	database, err := sql.Open("sqlite", filepath.Join(directory, databaseName))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := database.Exec(`
		CREATE TRIGGER reject_private_chunk_before_vectors
		BEFORE INSERT ON chunks
		WHEN NEW.generation_id = 'second'
		BEGIN
			SELECT RAISE(ABORT, 'PRIVATE_PREVECTOR_TRIGGER_CANARY');
		END`); err != nil {
		_ = database.Close()
		t.Fatalf("create pre-vector trigger error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close trigger database error = %v", err)
	}
	second := vectorGeneration(t, "repository", "second", first.ID, []source.Chunk{
		vectorChunk("second", "private/pre-vector.py", 2, source.LanguagePython, "PRIVATE_PREVECTOR_SOURCE_CANARY"),
	}, []index.VectorRecord{{ChunkID: "second", Values: embedding.Vector{12345.5, -6789.25}}})

	err = store.Replace(context.Background(), second)
	if err == nil {
		t.Fatal("Replace(pre-vector failure) error = nil")
	}
	for _, canary := range []string{
		"PRIVATE_PREVECTOR_TRIGGER_CANARY", "PRIVATE_PREVECTOR_SOURCE_CANARY", "private/pre-vector.py", "12345.5", "6789.25",
	} {
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("Replace(pre-vector failure) error exposes %q: %v", canary, err)
		}
	}
	active, activeErr := store.ActiveGeneration(context.Background(), first.RepositoryID)
	if activeErr != nil || active != first.ID {
		t.Fatalf("ActiveGeneration() = %q, %v; want preserved %q", active, activeErr, first.ID)
	}
}

func TestVectorBoundLoadSanitizesMalformedCanonicalRow(t *testing.T) {
	directory := t.TempDir()
	store := newVectorStore(t, directory)
	generation := vectorGeneration(t, "repository", "generation", "", []source.Chunk{
		vectorChunk("chunk", "private/load.py", 1, source.LanguagePython, "PRIVATE_LOAD_SOURCE_CANARY"),
	}, []index.VectorRecord{{ChunkID: "chunk", Values: embedding.Vector{1, 0}}})
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	database, err := sql.Open("sqlite", filepath.Join(directory, databaseName))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := database.Exec(`UPDATE chunks SET start_line = 'PRIVATE_STORED_LINE_CANARY'`); err != nil {
		_ = database.Close()
		t.Fatalf("corrupt canonical line error = %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close corruption database error = %v", err)
	}
	reader := bindVectorReader(t, store, generation.RepositoryID)

	_, err = reader.Load(context.Background(), generation.RepositoryID)
	if !errors.Is(err, index.ErrReindexRequired) {
		t.Fatalf("Load(corrupt canonical row) error = %v, want ErrReindexRequired", err)
	}
	for _, canary := range []string{"PRIVATE_STORED_LINE_CANARY", "PRIVATE_LOAD_SOURCE_CANARY", "private/load.py"} {
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("Load(corrupt canonical row) error exposes %q: %v", canary, err)
		}
	}
}

func TestExactSearchCancelsDuringQueryNormalizationBeforeReaderLock(t *testing.T) {
	const dimensions = 4096
	store := newVectorStore(t, t.TempDir())
	storedValues := make(embedding.Vector, dimensions)
	storedValues[0] = 1
	generation := vectorGeneration(t, "repository", "generation", "", []source.Chunk{
		vectorChunk("chunk", "normalization.py", 1, source.LanguagePython, "NORMALIZATION_SOURCE"),
	}, []index.VectorRecord{{ChunkID: "chunk", Values: storedValues}})
	generation.Dimensions = dimensions
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	reader := bindVectorReader(t, store, generation.RepositoryID)
	if err := reader.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	values := make(embedding.Vector, dimensions)
	for position := range values {
		values[position] = 1
	}
	query := vectorIndexQuery(generation, values, 1)
	ctx := newCancelOnErrCheckContext(2)

	_, err := reader.Search(ctx, query)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Search(cancel during normalization) error = %v, want context.Canceled before reader lock", err)
	}
}

func TestExactSearchCancelsDuringCandidateRanking(t *testing.T) {
	store := newVectorStore(t, t.TempDir())
	const candidateCount = 512
	chunks := make([]source.Chunk, candidateCount)
	records := make([]index.VectorRecord, candidateCount)
	for position := range chunks {
		id := fmt.Sprintf("chunk-%04d", position)
		chunks[position] = vectorChunk(id, fmt.Sprintf("pkg/%04d.py", candidateCount-position), position+1, source.LanguagePython, "RANKING_SOURCE")
		records[position] = index.VectorRecord{ChunkID: id, Values: embedding.Vector{1, float32(position + 1)}}
	}
	generation := vectorGeneration(t, "repository", "ranking", "", chunks, records)
	if err := store.Replace(context.Background(), generation); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	reader := bindVectorReader(t, store, generation.RepositoryID)
	if _, err := reader.Load(context.Background(), generation.RepositoryID); err != nil {
		t.Fatalf("Load(warm canonical cache) error = %v", err)
	}
	filteredQuery := vectorIndexQuery(generation, embedding.Vector{1, 1}, candidateCount)
	filteredQuery.Filter = search.Filter{PathPrefixes: []string{"not-ranked"}}
	baselineCtx := &errCountingContext{Context: context.Background()}
	candidates, err := reader.Search(baselineCtx, filteredQuery)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("Search(filtered baseline) = %#v, %v; want empty success", candidates, err)
	}
	query := vectorIndexQuery(generation, embedding.Vector{1, 1}, candidateCount)
	query.Filter = search.Filter{PathPrefixes: []string{"pkg"}}
	cancelCtx := newCancelOnErrCheckContext(baselineCtx.checks.Load() + 2)
	searchDone := make(chan exactSearchResult, 1)
	go func() {
		found, searchErr := reader.Search(cancelCtx, query)
		searchDone <- exactSearchResult{candidates: found, err: searchErr}
	}()
	select {
	case <-cancelCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Search did not observe cancellation during candidate ranking")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- reader.Close() }()
	var result exactSearchResult
	select {
	case result = <-searchDone:
	case <-time.After(time.Second):
		t.Fatal("Search held reader lock after cancellation during candidate ranking")
	}
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("Search(cancel during ranking) candidate count = %d, error = %v; want context.Canceled and no candidates", len(result.candidates), result.err)
	}
	if result.candidates != nil {
		t.Fatalf("Search(cancel during ranking) candidates = %#v, want nil", result.candidates)
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close(after canceled ranking) error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close remained blocked after canceled candidate ranking")
	}
}

func newVectorStore(t *testing.T, directory string) *Store {
	t.Helper()
	store, err := NewStore(directory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return store
}

func bindVectorReader(t *testing.T, store *Store, repositoryID string) *BoundReader {
	t.Helper()
	reader, err := store.BindActive(context.Background(), repositoryID)
	if err != nil {
		t.Fatalf("BindActive() error = %v", err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Errorf("BoundReader.Close() error = %v", err)
		}
	})
	return reader
}

func vectorIndexQuery(generation index.Generation, values embedding.Vector, limit int) vectorsearch.IndexQuery {
	return vectorsearch.IndexQuery{
		RepositoryID: generation.RepositoryID,
		GenerationID: generation.ID,
		Profile:      *generation.Profile,
		Dimensions:   generation.Dimensions,
		Metric:       generation.Metric,
		Vector:       values,
		Limit:        limit,
	}
}

func vectorGeneration(t *testing.T, repositoryID, generationID, baseGeneration string, chunks []source.Chunk, records []index.VectorRecord) index.Generation {
	t.Helper()
	corpus, err := source.NewCorpus(ingest.ScanPolicyVersion, chunks)
	if err != nil {
		t.Fatalf("NewCorpus() error = %v", err)
	}
	profile := &embedding.Profile{Fingerprint: "profile-fingerprint", Model: "profile-model"}
	return index.Generation{
		RepositoryID:      repositoryID,
		ID:                generationID,
		BaseGeneration:    baseGeneration,
		CorpusRevision:    corpus.Revision,
		ScanPolicyVersion: corpus.PolicyVersion,
		Chunks:            corpus.Chunks,
		Profile:           profile,
		Dimensions:        2,
		Metric:            index.VectorMetricCosine,
		Vectors:           records,
	}
}

func lexicalVectorGeneration(t *testing.T, repositoryID, generationID, chunkID, path string) index.Generation {
	t.Helper()
	chunk := vectorChunk(chunkID, path, 1, source.LanguagePython, "LEXICAL_ONLY_SOURCE")
	corpus, err := source.NewCorpus(ingest.ScanPolicyVersion, []source.Chunk{chunk})
	if err != nil {
		t.Fatalf("NewCorpus() error = %v", err)
	}
	return index.Generation{
		RepositoryID:      repositoryID,
		ID:                generationID,
		CorpusRevision:    corpus.Revision,
		ScanPolicyVersion: corpus.PolicyVersion,
		Chunks:            corpus.Chunks,
		Metric:            index.VectorMetricCosine,
	}
}

func lexicalVectorQuery(generation index.Generation) vectorsearch.IndexQuery {
	return vectorsearch.IndexQuery{
		RepositoryID: generation.RepositoryID,
		GenerationID: generation.ID,
		Profile:      embedding.Profile{Fingerprint: "expected", Model: "expected"},
		Dimensions:   2,
		Metric:       index.VectorMetricCosine,
		Vector:       embedding.Vector{1, 0},
		Limit:        1,
	}
}

func vectorChunk(id, path string, line int, language source.Language, text string) source.Chunk {
	return source.Chunk{
		ID:         id,
		Text:       text,
		Language:   language,
		SymbolName: "VectorSymbol",
		Reference: source.Reference{
			Path:      path,
			StartLine: line,
			EndLine:   line + 1,
		},
	}
}

func osReadFileAllowMissing(path string) ([]byte, error) {
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return payload, err
}

type errCountingContext struct {
	context.Context
	checks atomic.Int64
}

type exactSearchResult struct {
	candidates []vectorsearch.Candidate
	err        error
}

func (c *errCountingContext) Done() <-chan struct{} {
	return nil
}

func (c *errCountingContext) Err() error {
	c.checks.Add(1)
	return nil
}

type cancelOnErrCheckContext struct {
	context.Context
	cancelAt int64
	checks   atomic.Int64
	done     chan struct{}
	once     sync.Once
}

func newCancelOnErrCheckContext(cancelAt int64) *cancelOnErrCheckContext {
	return &cancelOnErrCheckContext{
		Context:  context.Background(),
		cancelAt: cancelAt,
		done:     make(chan struct{}),
	}
}

func (c *cancelOnErrCheckContext) Done() <-chan struct{} {
	return c.done
}

func (c *cancelOnErrCheckContext) Err() error {
	if c.checks.Add(1) < c.cancelAt {
		return nil
	}
	c.once.Do(func() { close(c.done) })
	return context.Canceled
}
