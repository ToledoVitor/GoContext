package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/embedding"
	"github.com/ToledoVitor/GoContext/internal/embedding/openaicompat"
	"github.com/ToledoVitor/GoContext/internal/index"
	indexsqlite "github.com/ToledoVitor/GoContext/internal/index/sqlite"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestRunIndexSearchSemanticEndToEndUsesCanonicalSQLiteGeneration(t *testing.T) {
	clearEmbeddingEnvironment(t)
	t.Setenv(embeddingAPIKeyEnv, "LOCAL_KEY_CANARY")

	var requestsMu sync.Mutex
	var requestInputs [][]string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/embeddings" {
			t.Errorf("embedding request = %s %s, want POST /v1/embeddings", request.Method, request.URL.Path)
		}
		var body struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode embedding request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		requestsMu.Lock()
		requestInputs = append(requestInputs, append([]string(nil), body.Input...))
		requestsMu.Unlock()

		data := make([]map[string]any, len(body.Input))
		for index, input := range body.Input {
			vector := []float64{1, 0}
			if strings.Contains(input, "authorize_session") {
				vector = []float64{0.8, 0.2}
			}
			if input == "credential boundary" {
				vector = []float64{1, 0}
			}
			data[index] = map[string]any{
				"object":    "embedding",
				"index":     index,
				"embedding": vector,
			}
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"object": "list",
			"data":   data,
			"model":  "fixture-model",
			"usage":  map[string]int{"prompt_tokens": 5, "total_tokens": 7},
		})
	}))
	t.Cleanup(server.Close)

	repository := t.TempDir()
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "src/security.py", "def authorize_session():\n    return True\n")
	writeCLIFile(t, repository, "tests/math.py", "def calculate_total():\n    return 42\n")

	semanticArgs := []string{
		"--semantic", "preferred",
		"--embedding-base-url", server.URL + "/v1",
		"--embedding-model", "fixture-model",
		"--embedding-dimensions", "2",
	}
	var indexOut bytes.Buffer
	var indexErr bytes.Buffer
	indexArgs := append([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite"}, semanticArgs...)
	indexArgs = append(indexArgs, repository)
	if code := run(indexArgs, &indexOut, &indexErr); code != 0 {
		t.Fatalf("run(index semantic) code = %d, want 0; stderr = %q", code, indexErr.String())
	}
	if got, want := indexOut.String(), "indexado: 2 arquivos, 2 símbolos, 2 chunks\nsemântica: status=indexed vetores=2 requests=1 tokens=7\n"; got != want {
		t.Fatalf("run(index semantic) stdout = %q, want %q", got, want)
	}
	if indexErr.Len() != 0 {
		t.Fatalf("run(index semantic) stderr = %q, want local-loopback silence", indexErr.String())
	}

	var searchOut bytes.Buffer
	var searchErr bytes.Buffer
	searchArgs := append([]string{
		"search", "--store", storeDirectory, "--index-backend", "auto", "--limit", "10",
		"--path-prefix", "src", "--path-prefix", "lib",
		"--language", "python", "--language", "python",
	}, semanticArgs...)
	searchArgs = append(searchArgs, repository, "credential", "boundary")
	if code := run(searchArgs, &searchOut, &searchErr); code != 0 {
		t.Fatalf("run(search semantic) code = %d, want 0; stderr = %q", code, searchErr.String())
	}
	if !strings.Contains(searchOut.String(), "src/security.py:1-2") ||
		!strings.Contains(searchOut.String(), "def authorize_session()") {
		t.Fatalf("run(search semantic) stdout = %q, want canonical security chunk", searchOut.String())
	}
	if strings.Contains(searchOut.String(), "tests/math.py") {
		t.Fatalf("run(search semantic) stdout = %q, want path-prefix exclusion", searchOut.String())
	}
	if searchErr.Len() != 0 {
		t.Fatalf("run(search semantic) stderr = %q, want local-loopback silence", searchErr.String())
	}

	requestsMu.Lock()
	gotInputs := append([][]string(nil), requestInputs...)
	requestsMu.Unlock()
	if len(gotInputs) != 2 || len(gotInputs[0]) != 2 || !reflect.DeepEqual(gotInputs[1], []string{"credential boundary"}) {
		t.Fatalf("embedding request inputs = %#v, want one document batch then one query", gotInputs)
	}
	for _, diagnostics := range []string{indexErr.String(), searchErr.String()} {
		for _, canary := range []string{"LOCAL_KEY_CANARY", "credential boundary", "authorize_session"} {
			if strings.Contains(diagnostics, canary) {
				t.Fatalf("semantic diagnostics expose %q: %q", canary, diagnostics)
			}
		}
	}
}

func TestRunIndexSearchSemanticFallbackAndRequiredPreserveGeneration(t *testing.T) {
	clearEmbeddingEnvironment(t)
	t.Setenv(embeddingAPIKeyEnv, "FALLBACK_KEY_CANARY")
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(writer, "REMOTE_BODY_CANARY", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	repository := t.TempDir()
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "fallback.py", "def lexical_fallback():\n    return True\n")
	providerFlags := []string{
		"--embedding-base-url", server.URL + "/v1",
		"--embedding-model", "fixture-model",
		"--embedding-dimensions", "2",
	}
	var preferredOut bytes.Buffer
	var preferredErr bytes.Buffer
	preferredArgs := append([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", "--semantic", "preferred"}, providerFlags...)
	preferredArgs = append(preferredArgs, repository)
	if code := run(preferredArgs, &preferredOut, &preferredErr); code != 0 {
		t.Fatalf("run(index preferred unavailable) code = %d, want 0; stderr = %q", code, preferredErr.String())
	}
	if got, want := preferredOut.String(), "indexado: 1 arquivos, 1 símbolos, 1 chunks\nsemântica: status=degraded vetores=0 requests=3 tokens=0\n"; got != want {
		t.Fatalf("run(index preferred unavailable) stdout = %q, want %q", got, want)
	}
	if got, want := preferredErr.String(), semanticDegradedWarning; got != want {
		t.Fatalf("run(index preferred unavailable) stderr = %q, want %q", got, want)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("preferred exhausted requests = %d, want 3", got)
	}

	var searchOut bytes.Buffer
	var searchErr bytes.Buffer
	searchArgs := append([]string{"search", "--store", storeDirectory, "--index-backend", "auto", "--semantic", "preferred"}, providerFlags...)
	searchArgs = append(searchArgs, repository, "lexical", "fallback")
	if code := run(searchArgs, &searchOut, &searchErr); code != 0 {
		t.Fatalf("run(search preferred lexical-only) code = %d, want 0; stderr = %q", code, searchErr.String())
	}
	if !strings.Contains(searchOut.String(), "fallback.py:1-2") {
		t.Fatalf("run(search preferred lexical-only) stdout = %q, want lexical hit", searchOut.String())
	}
	if got, want := searchErr.String(), semanticDegradedWarning; got != want {
		t.Fatalf("run(search preferred lexical-only) stderr = %q, want %q", got, want)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("lexical-only search made requests: got %d, want 3 total", got)
	}

	repositoryID := canonicalPath(t, repository)
	markerBefore, err := os.ReadFile(rollbackMarkerPath(storeDirectory, repositoryID))
	if err != nil {
		t.Fatalf("ReadFile(marker before required) error = %v", err)
	}
	store, err := indexsqlite.OpenExisting(storeDirectory)
	if err != nil {
		t.Fatalf("OpenExisting(before required) error = %v", err)
	}
	activeBefore, err := store.ActiveGeneration(context.Background(), repositoryID)
	if err != nil {
		t.Fatalf("ActiveGeneration(before required) error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(before required) error = %v", err)
	}

	writeCLIFile(t, repository, "fallback.py", "def replacement_content():\n    return False\n")
	var requiredOut bytes.Buffer
	var requiredErr bytes.Buffer
	requiredArgs := append([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", "--semantic", "required"}, providerFlags...)
	requiredArgs = append(requiredArgs, repository)
	if code := run(requiredArgs, &requiredOut, &requiredErr); code != 1 {
		t.Fatalf("run(index required unavailable) code = %d, want 1; stderr = %q", code, requiredErr.String())
	}
	if requiredOut.Len() != 0 {
		t.Fatalf("run(index required unavailable) stdout = %q, want empty", requiredOut.String())
	}
	for _, canary := range []string{"REMOTE_BODY_CANARY", "FALLBACK_KEY_CANARY", "replacement_content"} {
		if strings.Contains(requiredErr.String(), canary) {
			t.Fatalf("run(index required unavailable) stderr exposes %q: %q", canary, requiredErr.String())
		}
	}
	markerAfter, err := os.ReadFile(rollbackMarkerPath(storeDirectory, repositoryID))
	if err != nil {
		t.Fatalf("ReadFile(marker after required) error = %v", err)
	}
	if !bytes.Equal(markerAfter, markerBefore) {
		t.Fatalf("required failure changed rollback marker: before %q after %q", markerBefore, markerAfter)
	}
	store, err = indexsqlite.OpenExisting(storeDirectory)
	if err != nil {
		t.Fatalf("OpenExisting(after required) error = %v", err)
	}
	activeAfter, err := store.ActiveGeneration(context.Background(), repositoryID)
	if err != nil {
		t.Fatalf("ActiveGeneration(after required) error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(after required) error = %v", err)
	}
	if activeAfter != activeBefore {
		t.Fatalf("required failure active generation = %q, want preserved %q", activeAfter, activeBefore)
	}
}

func TestRunIndexSemanticMalformedResponseIsFatalWithoutPublication(t *testing.T) {
	clearEmbeddingEnvironment(t)
	t.Setenv(embeddingAPIKeyEnv, "MALFORMED_KEY_CANARY")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0,0]}],"model":"fixture-model","provider_body":"REMOTE_MALFORMED_CANARY","usage":{"total_tokens":1}}`))
	}))
	t.Cleanup(server.Close)
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "malformed.py", "def malformed_source_canary():\n    return True\n")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{
		"index", "--store", storeDirectory, "--index-backend", "sqlite",
		"--semantic", "preferred", "--embedding-base-url", server.URL + "/v1",
		"--embedding-model", "fixture-model", "--embedding-dimensions", "2",
		repository,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(index malformed) code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("run(index malformed) stdout = %q, want no publication report", stdout.String())
	}
	for _, canary := range []string{"REMOTE_MALFORMED_CANARY", "MALFORMED_KEY_CANARY", "malformed_source_canary"} {
		if strings.Contains(stderr.String(), canary) {
			t.Fatalf("run(index malformed) stderr exposes %q: %q", canary, stderr.String())
		}
	}
	repositoryID := canonicalPath(t, repository)
	store, err := indexsqlite.OpenExisting(storeDirectory)
	if err != nil {
		t.Fatalf("OpenExisting() error = %v", err)
	}
	_, activeErr := store.ActiveGeneration(context.Background(), repositoryID)
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !errors.Is(activeErr, index.ErrNotFound) {
		t.Fatalf("ActiveGeneration() error = %v, want ErrNotFound", activeErr)
	}
	if _, err := os.Stat(rollbackMarkerPath(storeDirectory, repositoryID)); !os.IsNotExist(err) {
		t.Fatalf("Stat(rollback marker) error = %v, want absent", err)
	}
}

func TestRunSearchSemanticProfileChangeFallsBackOnlyWhenPreferred(t *testing.T) {
	clearEmbeddingEnvironment(t)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		var body struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		data := make([]map[string]any, len(body.Input))
		for position := range body.Input {
			data[position] = map[string]any{"index": position, "embedding": []float64{1, 0}}
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"data": data, "model": "wire-model", "usage": map[string]int{"total_tokens": 1},
		})
	}))
	t.Cleanup(server.Close)
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "profile.py", "def profile_token():\n    return True\n")
	baseURLFlags := []string{"--embedding-base-url", server.URL + "/v1"}
	indexArgs := append([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", "--semantic", "required", "--embedding-model", "model-a", "--embedding-dimensions", "2"}, baseURLFlags...)
	indexArgs = append(indexArgs, repository)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run(indexArgs, &stdout, &stderr); code != 0 {
		t.Fatalf("run(index model-a) code = %d; stderr = %q", code, stderr.String())
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("index requests = %d, want 1", got)
	}

	for _, test := range []struct {
		name       string
		mode       string
		model      string
		dimensions string
		wantCode   int
		wantHit    bool
		wantWarn   bool
	}{
		{name: "preferred model change", mode: "preferred", model: "model-b", dimensions: "2", wantCode: 0, wantHit: true, wantWarn: true},
		{name: "preferred dimension change", mode: "preferred", model: "model-a", dimensions: "3", wantCode: 0, wantHit: true, wantWarn: true},
		{name: "required model change", mode: "required", model: "model-b", dimensions: "2", wantCode: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdout.Reset()
			stderr.Reset()
			args := append([]string{
				"search", "--store", storeDirectory, "--index-backend", "auto", "--semantic", test.mode,
				"--embedding-model", test.model, "--embedding-dimensions", test.dimensions,
			}, baseURLFlags...)
			args = append(args, repository, "profile", "token")
			code := run(args, &stdout, &stderr)
			if code != test.wantCode {
				t.Fatalf("run(search profile change) code = %d, want %d; stderr = %q", code, test.wantCode, stderr.String())
			}
			if got := strings.Contains(stdout.String(), "profile.py:1-2"); got != test.wantHit {
				t.Fatalf("run(search profile change) hit = %t, want %t; stdout = %q", got, test.wantHit, stdout.String())
			}
			if got := stderr.String() == semanticDegradedWarning; got != test.wantWarn {
				t.Fatalf("run(search profile change) warning-only = %t, want %t; stderr = %q", got, test.wantWarn, stderr.String())
			}
		})
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("profile preflight search requests = %d, want unchanged 1", got)
	}
}

func TestRunIndexSemanticParentCancellationDoesNotPublish(t *testing.T) {
	clearEmbeddingEnvironment(t)
	requestStarted := make(chan struct{}, 1)
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestStarted <- struct{}{}
		select {
		case <-request.Context().Done():
		case <-releaseHandler:
		}
	}))
	t.Cleanup(server.Close)
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "cancel.py", "def cancellation_canary():\n    return True\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runIndex(ctx, []string{
			"--store", storeDirectory, "--index-backend", "sqlite", "--semantic", "required",
			"--embedding-base-url", server.URL + "/v1", "--embedding-model", "fixture-model",
			"--embedding-dimensions", "2", repository,
		}, &stdout, &stderr)
	}()
	<-requestStarted
	cancel()
	code := <-done
	close(releaseHandler)
	if code != 1 {
		t.Fatalf("runIndex(canceled) code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("runIndex(canceled) stdout = %q, want empty", stdout.String())
	}
	repositoryID := canonicalPath(t, repository)
	store, err := indexsqlite.OpenExisting(storeDirectory)
	if err != nil {
		t.Fatalf("OpenExisting() error = %v", err)
	}
	_, activeErr := store.ActiveGeneration(context.Background(), repositoryID)
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !errors.Is(activeErr, index.ErrNotFound) {
		t.Fatalf("ActiveGeneration() error = %v, want ErrNotFound", activeErr)
	}
}

func TestRunIndexSemanticExternalEgressWarningIsFixedAndPrecedesFailure(t *testing.T) {
	clearEmbeddingEnvironment(t)
	t.Setenv(embeddingAPIKeyEnv, "EXTERNAL_INDEX_KEY_CANARY")
	server := newQuietTLSServer(t)
	externalURL := strings.Replace(server.URL, "127.0.0.1", "localhost", 1) + "/v1"
	repository := t.TempDir()
	storeDirectory := t.TempDir()
	writeCLIFile(t, repository, "egress.py", "def EXTERNAL_INDEX_SOURCE_CANARY():\n    return True\n")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{
		"index", "--store", storeDirectory, "--index-backend", "sqlite",
		"--semantic", "required", "--embedding-base-url", externalURL,
		"--embedding-model", "fixture-model", "--embedding-dimensions", "2", repository,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(index external egress) code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), externalIndexEgressWarning) || strings.Count(stderr.String(), externalIndexEgressWarning) != 1 {
		t.Fatalf("run(index external egress) stderr = %q, want one leading fixed disclosure", stderr.String())
	}
	for _, canary := range []string{"EXTERNAL_INDEX_KEY_CANARY", "EXTERNAL_INDEX_SOURCE_CANARY", "egress.py", externalURL} {
		if strings.Contains(stderr.String(), canary) {
			t.Fatalf("run(index external egress) stderr exposes %q: %q", canary, stderr.String())
		}
	}
}

func TestRunSearchSemanticExternalEgressWarningIsFixedAndPrecedesFailure(t *testing.T) {
	clearEmbeddingEnvironment(t)
	t.Setenv(embeddingAPIKeyEnv, "EXTERNAL_SEARCH_KEY_CANARY")
	server := newQuietTLSServer(t)
	externalURL := strings.Replace(server.URL, "127.0.0.1", "localhost", 1) + "/v1"
	client, err := openaicompat.New(openaicompat.Config{
		BaseURL: externalURL, Model: "fixture-model", Dimensions: 2,
	})
	if err != nil {
		t.Fatalf("New(external profile) error = %v", err)
	}
	repository := t.TempDir()
	repositoryID := canonicalPath(t, repository)
	storeDirectory := t.TempDir()
	chunk := source.Chunk{
		ID: "external-search", Text: "EXTERNAL_SEARCH_SOURCE_CANARY lexical token",
		Language:  source.LanguagePython,
		Reference: source.Reference{Path: "external-search.py", StartLine: 1, EndLine: 1},
	}
	corpus := mustCLICorpus(t, []source.Chunk{chunk})
	store, err := indexsqlite.NewStore(storeDirectory)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	profile := client.Profile()
	if err := store.Replace(context.Background(), index.Generation{
		RepositoryID: repositoryID, ID: "external-generation", CorpusRevision: corpus.Revision,
		ScanPolicyVersion: corpus.PolicyVersion, Chunks: corpus.Chunks, Profile: &profile,
		Dimensions: 2, Metric: index.VectorMetricCosine,
		Vectors: []index.VectorRecord{{ChunkID: chunk.ID, Values: embedding.Vector{1, 0}}},
	}); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{
		"search", "--store", storeDirectory, "--index-backend", "auto",
		"--semantic", "required", "--embedding-base-url", externalURL,
		"--embedding-model", "fixture-model", "--embedding-dimensions", "2",
		repository, "EXTERNAL_SEARCH_QUERY_CANARY",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run(search external egress) code = %d, want 1; stderr = %q", code, stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), externalSearchEgressWarning) || strings.Count(stderr.String(), externalSearchEgressWarning) != 1 {
		t.Fatalf("run(search external egress) stderr = %q, want one leading fixed disclosure", stderr.String())
	}
	for _, canary := range []string{
		"EXTERNAL_SEARCH_KEY_CANARY", "EXTERNAL_SEARCH_SOURCE_CANARY", "EXTERNAL_SEARCH_QUERY_CANARY",
		"external-search.py", externalURL,
	} {
		if strings.Contains(stderr.String(), canary) {
			t.Fatalf("run(search external egress) stderr exposes %q: %q", canary, stderr.String())
		}
	}
}

func newQuietTLSServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "REMOTE_TLS_BODY_CANARY", http.StatusInternalServerError)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}
