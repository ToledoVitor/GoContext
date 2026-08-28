package source

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const corpusRevisionVersion = "corpus-v1"

// Corpus is a complete, policy-bound set of canonical source chunks.
type Corpus struct {
	PolicyVersion string
	Revision      string
	Chunks        []Chunk
}

// NewCorpus validates chunks and derives a stable revision from the policy and
// sorted chunk IDs. The chunk order remains unchanged for callers.
func NewCorpus(policyVersion string, chunks []Chunk) (Corpus, error) {
	if strings.TrimSpace(policyVersion) == "" {
		return Corpus{}, fmt.Errorf("create source corpus: policy version is empty")
	}

	ids := make([]string, len(chunks))
	seen := make(map[string]struct{}, len(chunks))
	for index, chunk := range chunks {
		if chunk.ID == "" || chunk.Text == "" || !chunk.Reference.Valid() {
			return Corpus{}, fmt.Errorf("create source corpus: chunk %d has incomplete content or provenance", index)
		}
		if _, duplicate := seen[chunk.ID]; duplicate {
			return Corpus{}, fmt.Errorf("create source corpus: chunk %d duplicates an earlier ID", index)
		}
		seen[chunk.ID] = struct{}{}
		ids[index] = chunk.ID
	}
	sort.Strings(ids)

	digest := sha256.New()
	writeRevisionPart(digest, corpusRevisionVersion)
	writeRevisionPart(digest, policyVersion)
	for _, id := range ids {
		writeRevisionPart(digest, id)
	}

	return Corpus{
		PolicyVersion: policyVersion,
		Revision:      hex.EncodeToString(digest.Sum(nil)),
		Chunks:        append([]Chunk(nil), chunks...),
	}, nil
}

type revisionWriter interface {
	Write([]byte) (int, error)
}

func writeRevisionPart(writer revisionWriter, value string) {
	_, _ = writer.Write([]byte(fmt.Sprintf("%d:", len(value))))
	_, _ = writer.Write([]byte(value))
}
