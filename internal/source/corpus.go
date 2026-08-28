package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	corpusRevisionVersion  = "corpus-v1"
	corpusContextStride    = 256
	corpusHashWriteMaxSize = 64 * 1024
)

// Corpus is a complete, policy-bound set of canonical source chunks.
type Corpus struct {
	PolicyVersion string
	Revision      string
	Chunks        []Chunk
}

// NewCorpus validates chunks and derives a stable revision from the policy and
// sorted chunk IDs. The chunk order remains unchanged for callers.
func NewCorpus(policyVersion string, chunks []Chunk) (Corpus, error) {
	return NewCorpusContext(context.Background(), policyVersion, chunks)
}

// NewCorpusContext validates chunks and derives their revision while honoring
// cancellation through validation, ordering, and hashing work.
func NewCorpusContext(ctx context.Context, policyVersion string, chunks []Chunk) (Corpus, error) {
	if err := ctx.Err(); err != nil {
		return Corpus{}, fmt.Errorf("create source corpus: %w", err)
	}
	if strings.TrimSpace(policyVersion) == "" {
		return Corpus{}, fmt.Errorf("create source corpus: policy version is empty")
	}

	ids := make([]string, len(chunks))
	seen := make(map[string]struct{}, len(chunks))
	for index, chunk := range chunks {
		if index%corpusContextStride == 0 {
			if err := ctx.Err(); err != nil {
				return Corpus{}, fmt.Errorf("create source corpus: %w", err)
			}
		}
		if chunk.ID == "" || chunk.Text == "" || !chunk.Reference.Valid() {
			return Corpus{}, fmt.Errorf("create source corpus: chunk %d has incomplete content or provenance", index)
		}
		if _, duplicate := seen[chunk.ID]; duplicate {
			return Corpus{}, fmt.Errorf("create source corpus: chunk %d duplicates an earlier ID", index)
		}
		seen[chunk.ID] = struct{}{}
		ids[index] = chunk.ID
	}
	if err := sortStringsContext(ctx, ids); err != nil {
		return Corpus{}, fmt.Errorf("create source corpus: %w", err)
	}

	digest := sha256.New()
	if err := writeRevisionPartContext(ctx, digest, corpusRevisionVersion); err != nil {
		return Corpus{}, fmt.Errorf("create source corpus: %w", err)
	}
	if err := writeRevisionPartContext(ctx, digest, policyVersion); err != nil {
		return Corpus{}, fmt.Errorf("create source corpus: %w", err)
	}
	for _, id := range ids {
		if err := writeRevisionPartContext(ctx, digest, id); err != nil {
			return Corpus{}, fmt.Errorf("create source corpus: %w", err)
		}
	}

	canonicalChunks := make([]Chunk, len(chunks))
	for offset := 0; offset < len(chunks); offset += corpusContextStride {
		if err := ctx.Err(); err != nil {
			return Corpus{}, fmt.Errorf("create source corpus: %w", err)
		}
		end := offset + corpusContextStride
		if end > len(chunks) {
			end = len(chunks)
		}
		copy(canonicalChunks[offset:end], chunks[offset:end])
	}

	return Corpus{
		PolicyVersion: policyVersion,
		Revision:      hex.EncodeToString(digest.Sum(nil)),
		Chunks:        canonicalChunks,
	}, nil
}

type revisionWriter interface {
	Write([]byte) (int, error)
}

func writeRevisionPartContext(ctx context.Context, writer revisionWriter, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, _ = writer.Write([]byte(fmt.Sprintf("%d:", len(value))))
	for offset := 0; offset < len(value); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := offset + corpusHashWriteMaxSize
		if end > len(value) {
			end = len(value)
		}
		_, _ = writer.Write([]byte(value[offset:end]))
		offset = end
	}
	return nil
}

func sortStringsContext(ctx context.Context, values []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(values) < 2 {
		return nil
	}
	buffer := make([]string, len(values))
	sourceValues := values
	targetValues := buffer
	inOriginal := true
	for width := 1; ; width *= 2 {
		checks := 0
		for start := 0; start < len(values); {
			mid := boundedCorpusIndex(start, width, len(values))
			end := boundedCorpusIndex(mid, width, len(values))
			left, right, output := start, mid, start
			for left < mid || right < end {
				if checks%corpusContextStride == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				checks++
				switch {
				case right >= end || (left < mid && sourceValues[left] <= sourceValues[right]):
					targetValues[output] = sourceValues[left]
					left++
				default:
					targetValues[output] = sourceValues[right]
					right++
				}
				output++
			}
			start = end
		}
		sourceValues, targetValues = targetValues, sourceValues
		inOriginal = !inOriginal
		if width >= len(values)-width {
			break
		}
	}
	if !inOriginal {
		for offset := 0; offset < len(values); offset += corpusContextStride {
			if err := ctx.Err(); err != nil {
				return err
			}
			end := offset + corpusContextStride
			if end > len(values) {
				end = len(values)
			}
			copy(values[offset:end], sourceValues[offset:end])
		}
	}
	return ctx.Err()
}

func boundedCorpusIndex(start, width, length int) int {
	if width > length-start {
		return length
	}
	return start + width
}
