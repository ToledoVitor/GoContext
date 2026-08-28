package source

import (
	"reflect"
	"testing"
)

func TestNewCorpusDerivesStableRevisionFromPolicyAndSortedChunkIDs(t *testing.T) {
	first := testCorpusChunk("b", "b.py")
	second := testCorpusChunk("a", "a.py")

	corpus, err := NewCorpus("scanner-v2", []Chunk{first, second})
	if err != nil {
		t.Fatalf("NewCorpus() error = %v", err)
	}
	reordered, err := NewCorpus("scanner-v2", []Chunk{second, first})
	if err != nil {
		t.Fatalf("NewCorpus(reordered) error = %v", err)
	}
	otherPolicy, err := NewCorpus("scanner-v3", []Chunk{first, second})
	if err != nil {
		t.Fatalf("NewCorpus(other policy) error = %v", err)
	}

	if corpus.PolicyVersion != "scanner-v2" {
		t.Errorf("PolicyVersion = %q, want scanner-v2", corpus.PolicyVersion)
	}
	if corpus.Revision == "" {
		t.Fatal("Revision is empty")
	}
	if reordered.Revision != corpus.Revision {
		t.Errorf("reordered Revision = %q, want %q", reordered.Revision, corpus.Revision)
	}
	if otherPolicy.Revision == corpus.Revision {
		t.Errorf("other policy Revision = %q, want different revision", otherPolicy.Revision)
	}
	if !reflect.DeepEqual(corpus.Chunks, []Chunk{first, second}) {
		t.Fatalf("Chunks = %#v, want input order preserved", corpus.Chunks)
	}
}

func TestNewCorpusRejectsIncompleteOrDuplicateInput(t *testing.T) {
	valid := testCorpusChunk("valid", "app.py")
	tests := []struct {
		name   string
		policy string
		chunks []Chunk
	}{
		{name: "missing policy", chunks: []Chunk{valid}},
		{name: "missing ID", policy: "scanner-v2", chunks: []Chunk{{Text: "x", Reference: valid.Reference}}},
		{name: "missing text", policy: "scanner-v2", chunks: []Chunk{{ID: "x", Reference: valid.Reference}}},
		{name: "invalid reference", policy: "scanner-v2", chunks: []Chunk{{ID: "x", Text: "x"}}},
		{name: "duplicate ID", policy: "scanner-v2", chunks: []Chunk{valid, valid}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewCorpus(tt.policy, tt.chunks); err == nil {
				t.Fatal("NewCorpus() error = nil, want error")
			}
		})
	}
}

func testCorpusChunk(id, filePath string) Chunk {
	return Chunk{
		ID:        id,
		Text:      "source",
		Language:  LanguagePython,
		Reference: Reference{Path: filePath, StartLine: 1, EndLine: 1},
	}
}
