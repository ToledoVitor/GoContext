package main

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestRunIndexSearchJavaScriptTracerBulletIsDeterministic(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "src/constants.js", "export const answer = 42\n")
	writeCLIFile(t, repository, "src/view.JSX", "export const View = (props) => <main data-search=\"syntheticWidgetToken\" />\nconst plain = 42\n")
	writeCLIFile(t, repository, "src/data.json", `{"syntheticWidgetToken":"unsupported"}`)
	writeCLIFile(t, repository, "README.md", "syntheticWidgetToken unsupported\n")

	index := func() []source.Chunk {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run([]string{"index", "--store", storeDirectory, repository}, &stdout, &stderr); code != 0 {
			t.Fatalf("run(index JavaScript) code = %d, stderr = %q", code, stderr.String())
		}
		if got, want := stdout.String(), "indexado: 2 arquivos, 1 símbolos, 2 chunks\n"; got != want {
			t.Fatalf("run(index JavaScript) stdout = %q, want %q", got, want)
		}
		if stderr.Len() != 0 {
			t.Fatalf("run(index JavaScript) stderr = %q, want empty", stderr.String())
		}
		store, err := localstore.OpenExisting(storeDirectory)
		if err != nil {
			t.Fatalf("OpenExisting() error = %v", err)
		}
		chunks, err := store.Load(context.Background(), canonicalPath(t, repository))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		return chunks
	}

	first := index()
	second := index()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated JavaScript indexing changed chunks:\nfirst = %#v\nsecond = %#v", first, second)
	}
	wantChunks := []source.Chunk{
		{
			ID:        "0bceeae956b9612388d0af117dd7f8da82aa6063cf382d4a29f25dc8ae1e430b",
			Text:      "export const answer = 42",
			Language:  source.LanguageJavaScript,
			Reference: source.Reference{Path: "src/constants.js", StartLine: 1, EndLine: 1},
		},
		{
			ID:         "18ede875dffe996b526723923c02310d08aa31b987d505f661b54cec87f841a1",
			Text:       "export const View = (props) => <main data-search=\"syntheticWidgetToken\" />\nconst plain = 42",
			Language:   source.LanguageJavaScript,
			SymbolName: "View",
			Reference:  source.Reference{Path: "src/view.JSX", StartLine: 1, EndLine: 2},
		},
	}
	if len(first) != len(wantChunks) {
		t.Fatalf("stored JavaScript chunks = %#v, want two", first)
	}
	for index := range wantChunks {
		if first[index] != wantChunks[index] {
			t.Errorf("stored JavaScript chunk %d = %#v, want exact deterministic %#v", index, first[index], wantChunks[index])
		}
		if strings.Contains(first[index].Text, "unsupported") {
			t.Fatalf("stored JavaScript chunk %d contains unsupported-file fallback", index)
		}
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"search", "--store", storeDirectory, "--language", "javascript",
		repository, "synthetic", "widget", "token",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(search JavaScript) code = %d, stderr = %q", code, stderr.String())
	}
	wantOutput := "0.600 src/view.JSX:1-2 View\n" + wantChunks[1].Text + "\n"
	if got := stdout.String(); got != wantOutput {
		t.Fatalf("run(search JavaScript) stdout = %q, want exact citation %q", got, wantOutput)
	}
	if stderr.Len() != 0 {
		t.Fatalf("run(search JavaScript) stderr = %q, want empty", stderr.String())
	}
}
