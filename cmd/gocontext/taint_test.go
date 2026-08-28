package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	indexsqlite "github.com/ToledoVitor/GoContext/internal/index/sqlite"
	"github.com/ToledoVitor/GoContext/internal/ingest/filesystem"
	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
	"github.com/ToledoVitor/GoContext/internal/source"
)

const (
	cliTaintSecurityCanary     = "CLI_TAINT_SECURITY_CANARY_TASK13"
	cliTaintDependencyCanary   = "CLI_TAINT_DEPENDENCY_CANARY_TASK13"
	cliTaintNestedCanary       = "CLI_TAINT_NESTED_CANARY_TASK13"
	cliTaintSymlinkCanary      = "CLI_TAINT_SYMLINK_CANARY_TASK13"
	cliTaintBinaryCanary       = "CLI_TAINT_BINARY_CANARY_TASK13"
	cliTaintTooLargeCanary     = "CLI_TAINT_TOO_LARGE_CANARY_TASK13"
	cliTaintGeneratedCanary    = "CLI_TAINT_GENERATED_CANARY_TASK13"
	cliTaintUnsupportedCanary  = "CLI_TAINT_UNSUPPORTED_CANARY_TASK13"
	cliTaintInvalidUTF8Canary  = "CLI_TAINT_INVALID_UTF8_CANARY_TASK13"
	cliTaintPEMCanary          = "CLI_TAINT_PEM_CONTENT_CANARY_TASK13"
	cliTaintPythonSecretCanary = "CLI_TAINT_PYTHON_SECRET_CANARY_TASK13"
	cliTaintTypedSecretCanary  = "CLI_TAINT_TYPED_SECRET_CANARY_TASK13"
	cliTaintControlPathCanary  = "CLI_TAINT_CONTROL_PATH_CANARY_TASK13"
	cliTaintProviderKey        = "CLI_TAINT_PROVIDER_KEY_TASK13"
	cliTaintProviderBody       = "CLI_TAINT_PROVIDER_BODY_TASK13"
	cliTaintMalformedBody      = "CLI_TAINT_MALFORMED_BODY_TASK13"
	cliTaintQueryCanary        = "CLI_TAINT_QUERY_EGRESS_CANARY_TASK13"
	cliTaintReplacement        = "CLI_SAFE_UNPUBLISHED_REPLACEMENT_TASK13"
)

type cliTaintFixture struct {
	repository string
	forbidden  []string
}

type taintHTTPRequest struct {
	Method    string
	Path      string
	Host      string
	Header    http.Header
	Body      []byte
	ReadError string
}

type taintHTTPCapture struct {
	mu       sync.Mutex
	requests []taintHTTPRequest
}

func TestRunSnapshotOffTaintNeverReachesHTTPStoreOrOutput(t *testing.T) {
	clearEmbeddingEnvironment(t)
	fixture := newCLITaintFixture(t)
	storeDirectory := t.TempDir()
	server, capture := newNumericLoopbackTaintServer(t, func(writer http.ResponseWriter, _ taintHTTPRequest, _ int) {
		writeValidTaintEmbeddings(writer, nil)
	})
	t.Setenv(embeddingBaseURLEnv, server.URL+"/v1")
	t.Setenv(embeddingModelEnv, "task13-model")
	t.Setenv(embeddingAPIKeyEnv, cliTaintProviderKey)

	indexCode, indexOut, indexErr := runTaintCommand(t, "index", "--store", storeDirectory, fixture.repository)
	searchCode, searchOut, searchErr := runTaintCommand(
		t, "search", "--store", storeDirectory, fixture.repository, "permitted", "python", "lookup",
	)
	server.Close()

	if indexCode != 0 || indexOut != "indexado: 2 arquivos, 2 símbolos, 2 chunks\n" || indexErr != "" {
		t.Fatalf("snapshot/off index = code %d stdout %q stderr %q", indexCode, indexOut, indexErr)
	}
	assertPermittedSearchOutput(t, searchCode, searchOut, searchErr)
	if requests := capture.snapshot(); len(requests) != 0 {
		t.Fatalf("snapshot/off HTTP requests = %d, want zero", len(requests))
	}

	chunks := loadSnapshotTaintChunks(t, storeDirectory, fixture.repository)
	assertPermittedTaintChunks(t, chunks, fixture.forbidden)
	assertCLITaintSinksClean(t, fixture, []string{cliTaintProviderKey}, indexOut, indexErr, searchOut, searchErr)
	assertTaintStoreClean(t, storeDirectory, appendCopy(fixture.forbidden, cliTaintProviderKey))
}

func TestRunSQLiteOffTaintNeverReachesHTTPStoreOrOutput(t *testing.T) {
	clearEmbeddingEnvironment(t)
	fixture := newCLITaintFixture(t)
	storeDirectory := t.TempDir()
	server, capture := newNumericLoopbackTaintServer(t, func(writer http.ResponseWriter, _ taintHTTPRequest, _ int) {
		writeValidTaintEmbeddings(writer, nil)
	})
	t.Setenv(embeddingBaseURLEnv, server.URL+"/v1")
	t.Setenv(embeddingModelEnv, "task13-model")
	t.Setenv(embeddingAPIKeyEnv, cliTaintProviderKey)

	indexCode, indexOut, indexErr := runTaintCommand(
		t, "index", "--store", storeDirectory, "--index-backend", "sqlite", fixture.repository,
	)
	searchCode, searchOut, searchErr := runTaintCommand(
		t, "search", "--store", storeDirectory, "--index-backend", "auto",
		fixture.repository, "permitted", "python", "lookup",
	)
	server.Close()

	if indexCode != 0 || indexOut != "indexado: 2 arquivos, 2 símbolos, 2 chunks\n" || indexErr != "" {
		t.Fatalf("SQLite/off index = code %d stdout %q stderr %q", indexCode, indexOut, indexErr)
	}
	assertPermittedSearchOutput(t, searchCode, searchOut, searchErr)
	if requests := capture.snapshot(); len(requests) != 0 {
		t.Fatalf("SQLite/off HTTP requests = %d, want zero", len(requests))
	}

	chunks, _ := loadSQLiteTaintChunks(t, storeDirectory, fixture.repository)
	assertPermittedTaintChunks(t, chunks, fixture.forbidden)
	assertCLITaintSinksClean(t, fixture, []string{cliTaintProviderKey}, indexOut, indexErr, searchOut, searchErr)
	assertTaintStoreClean(t, storeDirectory, appendCopy(fixture.forbidden, cliTaintProviderKey))
}

func TestRunSQLitePreferredRetriesOnlyPermittedCorpusAndFallsBackLexically(t *testing.T) {
	clearEmbeddingEnvironment(t)
	fixture := newCLITaintFixture(t)
	storeDirectory := t.TempDir()
	server, capture := newNumericLoopbackTaintServer(t, func(writer http.ResponseWriter, _ taintHTTPRequest, _ int) {
		writer.Header().Set("Retry-After", "0")
		http.Error(writer, cliTaintProviderBody, http.StatusServiceUnavailable)
	})
	t.Setenv(embeddingAPIKeyEnv, cliTaintProviderKey)
	providerFlags := taintProviderFlags(server.URL)

	indexArgs := append([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", "--semantic", "preferred"}, providerFlags...)
	indexArgs = append(indexArgs, fixture.repository)
	indexCode, indexOut, indexErr := runTaintCommand(t, indexArgs...)
	searchArgs := append([]string{"search", "--store", storeDirectory, "--index-backend", "auto", "--semantic", "preferred"}, providerFlags...)
	searchArgs = append(searchArgs, fixture.repository, "permitted", "python", "lookup")
	searchCode, searchOut, searchErr := runTaintCommand(t, searchArgs...)
	server.Close()

	if indexCode != 0 || indexOut != "indexado: 2 arquivos, 2 símbolos, 2 chunks\nsemântica: status=degraded vetores=0 requests=3 tokens=0\n" ||
		indexErr != semanticDegradedWarning {
		t.Fatalf("SQLite/preferred index = code %d stdout %q stderr %q", indexCode, indexOut, indexErr)
	}
	if searchCode != 0 || searchErr != semanticDegradedWarning {
		t.Fatalf("SQLite/preferred search = code %d stdout %q stderr %q", searchCode, searchOut, searchErr)
	}
	if !strings.Contains(searchOut, "safe/allowed.py:1-2") || !strings.Contains(searchOut, "SAFE_PYTHON_SEARCH_TOKEN") {
		t.Fatalf("SQLite/preferred search stdout = %q, want lexical permitted hit", searchOut)
	}

	requests := capture.snapshot()
	if len(requests) != 3 {
		t.Fatalf("SQLite/preferred requests = %d, want three temporary attempts", len(requests))
	}
	assertTaintHTTPRequestsClean(t, requests, fixture.forbidden)
	for position, request := range requests {
		inputs := decodeTaintInputs(t, request.Body)
		if len(inputs) != 2 || !strings.Contains(inputs[0], "SAFE_PYTHON_SEARCH_TOKEN") ||
			!strings.Contains(inputs[1], "SAFE_TYPESCRIPT_SEARCH_TOKEN") {
			t.Fatalf("preferred request %d inputs = %#v, want only two permitted chunks", position, inputs)
		}
	}

	chunks, _ := loadSQLiteTaintChunks(t, storeDirectory, fixture.repository)
	assertPermittedTaintChunks(t, chunks, fixture.forbidden)
	extra := []string{cliTaintProviderKey, cliTaintProviderBody}
	assertCLITaintSinksClean(t, fixture, extra, indexOut, indexErr, searchOut, searchErr)
	assertTaintStoreClean(t, storeDirectory, appendCopy(fixture.forbidden, extra...))
}

func TestRunSQLiteRequiredSendsOnlyPermittedDocumentsAndExplicitQuery(t *testing.T) {
	clearEmbeddingEnvironment(t)
	fixture := newCLITaintFixture(t)
	storeDirectory := t.TempDir()
	server, capture := newNumericLoopbackTaintServer(t, func(writer http.ResponseWriter, request taintHTTPRequest, _ int) {
		writeValidTaintEmbeddings(writer, request.Body)
	})
	t.Setenv(embeddingAPIKeyEnv, cliTaintProviderKey)
	providerFlags := taintProviderFlags(server.URL)

	indexArgs := append([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", "--semantic", "required"}, providerFlags...)
	indexArgs = append(indexArgs, fixture.repository)
	indexCode, indexOut, indexErr := runTaintCommand(t, indexArgs...)
	searchArgs := append([]string{"search", "--store", storeDirectory, "--index-backend", "auto", "--semantic", "required"}, providerFlags...)
	searchArgs = append(searchArgs, fixture.repository, cliTaintQueryCanary)
	searchCode, searchOut, searchErr := runTaintCommand(t, searchArgs...)
	server.Close()

	if indexCode != 0 || indexOut != "indexado: 2 arquivos, 2 símbolos, 2 chunks\nsemântica: status=indexed vetores=2 requests=1 tokens=7\n" || indexErr != "" {
		t.Fatalf("SQLite/required index = code %d stdout %q stderr %q", indexCode, indexOut, indexErr)
	}
	if searchCode != 0 || searchErr != "" || !strings.Contains(searchOut, "safe/allowed.py:1-2") {
		t.Fatalf("SQLite/required search = code %d stdout %q stderr %q", searchCode, searchOut, searchErr)
	}

	requests := capture.snapshot()
	if len(requests) != 2 {
		t.Fatalf("SQLite/required requests = %d, want one document and one query request", len(requests))
	}
	assertTaintHTTPRequestsClean(t, requests, fixture.forbidden)
	documentInputs := decodeTaintInputs(t, requests[0].Body)
	if len(documentInputs) != 2 || !strings.Contains(documentInputs[0], "SAFE_PYTHON_SEARCH_TOKEN") ||
		!strings.Contains(documentInputs[1], "SAFE_TYPESCRIPT_SEARCH_TOKEN") ||
		strings.Contains(string(requests[0].Body), cliTaintQueryCanary) {
		t.Fatalf("required document inputs = %#v, want only permitted corpus", documentInputs)
	}
	if queryInputs := decodeTaintInputs(t, requests[1].Body); !reflect.DeepEqual(queryInputs, []string{cliTaintQueryCanary}) {
		t.Fatalf("required query inputs = %#v, want explicit query canary only", queryInputs)
	}
	for _, request := range requests {
		if got := request.Header.Get("Authorization"); got != "Bearer "+cliTaintProviderKey {
			t.Fatalf("Authorization = %q, want synthetic configured key", got)
		}
	}

	chunks, _ := loadSQLiteTaintChunks(t, storeDirectory, fixture.repository)
	assertPermittedTaintChunks(t, chunks, fixture.forbidden)
	assertCLITaintSinksClean(t, fixture, []string{cliTaintProviderKey, cliTaintQueryCanary}, indexOut, indexErr, searchOut, searchErr)
	assertTaintStoreClean(t, storeDirectory, appendCopy(fixture.forbidden, cliTaintProviderKey, cliTaintQueryCanary))
}

func TestRunSQLiteRequiredRetryThenMalformedResponsePreservesPriorGeneration(t *testing.T) {
	clearEmbeddingEnvironment(t)
	fixture := newCLITaintFixture(t)
	storeDirectory := t.TempDir()
	var failurePhase atomic.Bool
	var failureAttempts atomic.Int64
	server, capture := newNumericLoopbackTaintServer(t, func(writer http.ResponseWriter, request taintHTTPRequest, _ int) {
		if !failurePhase.Load() {
			writeValidTaintEmbeddings(writer, request.Body)
			return
		}
		attempt := failureAttempts.Add(1)
		if attempt < 3 {
			writer.Header().Set("Retry-After", "0")
			http.Error(writer, cliTaintProviderBody, http.StatusTooManyRequests)
			return
		}
		writeMalformedTaintEmbeddings(writer, request.Body)
	})
	t.Setenv(embeddingAPIKeyEnv, cliTaintProviderKey)
	providerFlags := taintProviderFlags(server.URL)
	baselineArgs := append([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", "--semantic", "required"}, providerFlags...)
	baselineArgs = append(baselineArgs, fixture.repository)

	baselineCode, baselineOut, baselineErr := runTaintCommand(t, baselineArgs...)
	if baselineCode != 0 || baselineErr != "" {
		server.Close()
		t.Fatalf("required baseline = code %d stdout %q stderr %q", baselineCode, baselineOut, baselineErr)
	}
	baselineChunks, baselineGeneration := loadSQLiteTaintChunks(t, storeDirectory, fixture.repository)
	assertPermittedTaintChunks(t, baselineChunks, fixture.forbidden)
	baselineSnapshot := loadSnapshotTaintChunks(t, storeDirectory, fixture.repository)
	assertPermittedTaintChunks(t, baselineSnapshot, fixture.forbidden)
	markerPath := rollbackMarkerPath(storeDirectory, canonicalPath(t, fixture.repository))
	markerBefore, err := os.ReadFile(markerPath)
	if err != nil {
		server.Close()
		t.Fatalf("ReadFile(marker before failure) error = %v", err)
	}

	writeCLIFile(t, fixture.repository, "safe/allowed.py", "def unpublished_replacement():\n    return \""+cliTaintReplacement+"\"\n")
	failurePhase.Store(true)
	failureCode, failureOut, failureErr := runTaintCommand(t, baselineArgs...)
	searchCode, searchOut, searchErr := runTaintCommand(
		t, "search", "--store", storeDirectory, "--index-backend", "auto",
		fixture.repository, "permitted", "python", "lookup",
	)
	server.Close()

	if failureCode != 1 || failureOut != "" || failureErr != "indexar repositório: falha na indexação SQLite\n" {
		t.Fatalf("required retry/malformed = code %d stdout %q stderr %q", failureCode, failureOut, failureErr)
	}
	assertPermittedSearchOutput(t, searchCode, searchOut, searchErr)
	markerAfter, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("ReadFile(marker after failure) error = %v", err)
	}
	if !bytes.Equal(markerAfter, markerBefore) {
		t.Fatal("required retry/malformed failure changed the rollback marker")
	}
	afterChunks, afterGeneration := loadSQLiteTaintChunks(t, storeDirectory, fixture.repository)
	if afterGeneration != baselineGeneration || !reflect.DeepEqual(afterChunks, baselineChunks) {
		t.Fatalf("required failure generation/chunks changed: generation %q -> %q", baselineGeneration, afterGeneration)
	}
	afterSnapshot := loadSnapshotTaintChunks(t, storeDirectory, fixture.repository)
	if !reflect.DeepEqual(afterSnapshot, baselineSnapshot) {
		t.Fatal("required retry/malformed failure changed the rollback snapshot")
	}

	requests := capture.snapshot()
	if len(requests) != 4 || failureAttempts.Load() != 3 {
		t.Fatalf("required baseline/failure requests = %d with %d failed attempts, want 4 and 3", len(requests), failureAttempts.Load())
	}
	assertTaintHTTPRequestsClean(t, requests, fixture.forbidden)
	for _, request := range requests[1:] {
		inputs := decodeTaintInputs(t, request.Body)
		if len(inputs) != 2 || !strings.Contains(inputs[0], cliTaintReplacement) ||
			!strings.Contains(inputs[1], "SAFE_TYPESCRIPT_SEARCH_TOKEN") {
			t.Fatalf("failed required request inputs = %#v, want only replacement permitted corpus", inputs)
		}
	}
	extra := []string{cliTaintProviderKey, cliTaintProviderBody, cliTaintMalformedBody, cliTaintReplacement}
	assertCLITaintSinksClean(t, fixture, extra, failureOut, failureErr, searchOut, searchErr)
	assertTaintStoreClean(t, storeDirectory, appendCopy(fixture.forbidden, extra...))
}

func TestRunIndexScannerFailurePublishesNothingAndMakesNoProviderRequest(t *testing.T) {
	clearEmbeddingEnvironment(t)
	repository := t.TempDir()
	writeCLIFile(t, repository, "00-safe.py", "def safe_before_failure():\n    return True\n")
	unsafeName := "zz-" + cliTaintControlPathCanary + "\u200b.py"
	unsafePath := filepath.Join(repository, unsafeName)
	if err := os.WriteFile(unsafePath, []byte(cliTaintControlPathCanary), 0o600); err != nil {
		t.Skipf("synthetic filesystem does not permit the control-path fixture: %v", err)
	}
	storeDirectory := filepath.Join(t.TempDir(), "uncreated-store")
	server, capture := newNumericLoopbackTaintServer(t, func(writer http.ResponseWriter, request taintHTTPRequest, _ int) {
		writeValidTaintEmbeddings(writer, request.Body)
	})
	t.Setenv(embeddingAPIKeyEnv, cliTaintProviderKey)
	args := append([]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", "--semantic", "required"}, taintProviderFlags(server.URL)...)
	args = append(args, repository)
	code, stdout, stderr := runTaintCommand(t, args...)
	server.Close()

	if code != 1 || stdout != "" || !strings.Contains(stderr, "invalid-path [ref:") {
		t.Fatalf("scanner failure = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if requests := capture.snapshot(); len(requests) != 0 {
		t.Fatalf("scanner failure provider requests = %d, want zero", len(requests))
	}
	if _, err := os.Stat(storeDirectory); !os.IsNotExist(err) {
		t.Fatalf("scanner failure store stat error = %v, want no store publication", err)
	}
	assertNoCLITaintValue(t, "scanner failure output", []string{stdout, stderr}, []string{
		cliTaintControlPathCanary, unsafeName, unsafePath, canonicalPath(t, repository), cliTaintProviderKey,
	})
}

func newCLITaintFixture(t *testing.T) cliTaintFixture {
	t.Helper()
	repository := t.TempDir()
	outside := t.TempDir()
	writeCLIFile(t, repository, "safe/allowed.py", "def permitted_python_lookup():\n    return \"SAFE_PYTHON_SEARCH_TOKEN\"\n")
	writeCLIFile(t, repository, "safe/allowed.ts", "export function permittedTypeScriptLookup() {\n  return \"SAFE_TYPESCRIPT_SEARCH_TOKEN\"\n}\n")

	for _, path := range []string{
		".env", ".env.local", ".env.ts", ".git/config", ".github/workflows/ci.ts",
		"credentials.py", "secret.ts", "material.pem", "certificate.crt",
	} {
		writeCLIFile(t, repository, path, cliTaintSecurityCanary+"\n")
	}
	for _, path := range []string{
		"node_modules/dependency.ts", "vendor/vendor.py", "build/output.ts", ".cache/cache.py",
	} {
		writeCLIFile(t, repository, path, cliTaintDependencyCanary+"\n")
	}
	writeCLIFile(t, repository, "nested-repository/.git/config", cliTaintNestedCanary+"\n")
	writeCLIFile(t, repository, "nested-repository/00-child.py", "def "+cliTaintNestedCanary+"():\n    return True\n")
	writeCLIFile(t, repository, "client.generated.ts", cliTaintGeneratedCanary+"\n")
	writeCLIFile(t, repository, "generated-header.py", "# Code generated for "+cliTaintGeneratedCanary+". DO NOT EDIT.\nVALUE = 1\n")
	writeCLIBytes(t, repository, "binary.py", append([]byte(cliTaintBinaryCanary), 0, 'x'))
	oversized := append([]byte(cliTaintTooLargeCanary), bytes.Repeat([]byte{'x'}, int(filesystem.DefaultMaxFileSize)+1)...)
	writeCLIBytes(t, repository, "oversized.ts", oversized)
	writeCLIBytes(t, repository, "invalid-utf8.py", append([]byte(cliTaintInvalidUTF8Canary), 0xff))
	writeCLIFile(t, repository, "opaque.CLI_TAINT_UNSUPPORTED_NAME_TASK13", cliTaintUnsupportedCanary+"\n")
	writeCLIFile(t, repository, "pem-material.py", "MATERIAL = '''-----BEGIN PRIVATE KEY-----\n"+cliTaintPEMCanary+"\n-----END PRIVATE KEY-----'''\n")
	writeCLIFile(t, repository, "embedded-secret.py", "safe = \"x\"; password: str = \""+cliTaintPythonSecretCanary+"\"\n")
	writeCLIFile(t, repository, "embedded-secret.ts", "const safe = \"x\"; const token: string = \""+cliTaintTypedSecretCanary+"\"\n")

	forbidden := []string{
		cliTaintSecurityCanary, cliTaintDependencyCanary, cliTaintNestedCanary, cliTaintBinaryCanary,
		cliTaintTooLargeCanary, cliTaintGeneratedCanary, cliTaintUnsupportedCanary, cliTaintInvalidUTF8Canary,
		cliTaintPEMCanary, cliTaintPythonSecretCanary, cliTaintTypedSecretCanary,
		".env", ".env.local", ".env.ts", ".git/config", ".github/workflows/ci.ts",
		"credentials.py", "secret.ts", "material.pem", "certificate.crt",
		"node_modules/dependency.ts", "vendor/vendor.py", "build/output.ts", ".cache/cache.py",
		"nested-repository/.git/config", "nested-repository/00-child.py", "client.generated.ts",
		"generated-header.py", "binary.py", "oversized.ts", "invalid-utf8.py",
		"opaque.CLI_TAINT_UNSUPPORTED_NAME_TASK13", "pem-material.py", "embedded-secret.py", "embedded-secret.ts",
	}
	if runtime.GOOS != "windows" {
		writeCLIFile(t, outside, "outside.py", cliTaintSymlinkCanary+"\n")
		writeCLIFile(t, repository, ".env.internal-link", cliTaintSymlinkCanary+"\n")
		if err := os.Symlink(filepath.Join(outside, "outside.py"), filepath.Join(repository, "external-link.py")); err != nil {
			t.Fatalf("Symlink(external) error = %v", err)
		}
		if err := os.Symlink(".env.internal-link", filepath.Join(repository, "internal-link.ts")); err != nil {
			t.Fatalf("Symlink(internal) error = %v", err)
		}
		forbidden = append(
			forbidden,
			cliTaintSymlinkCanary,
			".env.internal-link",
			"external-link.py",
			"internal-link.ts",
			filepath.Join(outside, "outside.py"),
		)
	}
	return cliTaintFixture{repository: repository, forbidden: forbidden}
}

func newNumericLoopbackTaintServer(
	t *testing.T,
	respond func(http.ResponseWriter, taintHTTPRequest, int),
) (*httptest.Server, *taintHTTPCapture) {
	t.Helper()
	capture := &taintHTTPCapture{}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen(loopback) error = %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, readErr := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		record := taintHTTPRequest{
			Method: request.Method, Path: request.URL.Path, Host: request.Host,
			Header: request.Header.Clone(), Body: append([]byte(nil), body...),
		}
		if readErr != nil {
			record.ReadError = readErr.Error()
		}
		ordinal := capture.add(record)
		respond(writer, record, ordinal)
	}))
	if err := server.Listener.Close(); err != nil {
		_ = listener.Close()
		t.Fatalf("Close(default httptest listener) error = %v", err)
	}
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("Parse(server URL) error = %v", err)
	}
	address := net.ParseIP(parsed.Hostname())
	if address == nil || !address.IsLoopback() {
		t.Fatalf("server host = %q, want numeric loopback", parsed.Hostname())
	}
	return server, capture
}

func (capture *taintHTTPCapture) add(request taintHTTPRequest) int {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.requests = append(capture.requests, request)
	return len(capture.requests)
}

func (capture *taintHTTPCapture) snapshot() []taintHTTPRequest {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	requests := make([]taintHTTPRequest, len(capture.requests))
	for index, request := range capture.requests {
		requests[index] = request
		requests[index].Header = request.Header.Clone()
		requests[index].Body = append([]byte(nil), request.Body...)
	}
	return requests
}

func writeValidTaintEmbeddings(writer http.ResponseWriter, body []byte) {
	inputs := decodeTaintInputsResponse(writer, body)
	if inputs == nil {
		return
	}
	data := make([]map[string]any, len(inputs))
	for index, input := range inputs {
		vector := []float64{1, 0}
		if strings.Contains(input, "SAFE_TYPESCRIPT_SEARCH_TOKEN") {
			vector = []float64{0, 1}
		}
		data[index] = map[string]any{"index": index, "embedding": vector}
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"data": data, "model": "task13-model", "usage": map[string]int{"total_tokens": 7},
	})
}

func writeMalformedTaintEmbeddings(writer http.ResponseWriter, body []byte) {
	inputs := decodeTaintInputsResponse(writer, body)
	if inputs == nil {
		return
	}
	data := make([]map[string]any, len(inputs))
	for index := range inputs {
		data[index] = map[string]any{"index": index, "embedding": []float64{0, 0}}
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"data": data, "provider_body": cliTaintMalformedBody, "usage": map[string]int{"total_tokens": 99},
	})
}

func decodeTaintInputsResponse(writer http.ResponseWriter, body []byte) []string {
	if len(body) == 0 {
		return []string{}
	}
	var request struct {
		Input []string `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(writer, "invalid synthetic request", http.StatusBadRequest)
		return nil
	}
	return request.Input
}

func taintProviderFlags(serverURL string) []string {
	return []string{
		"--embedding-base-url", serverURL + "/v1",
		"--embedding-model", "task13-model",
		"--embedding-dimensions", "2",
	}
}

func runTaintCommand(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func assertPermittedSearchOutput(t *testing.T, code int, stdout, stderr string) {
	t.Helper()
	if code != 0 || stderr != "" || !strings.Contains(stdout, "safe/allowed.py:1-2") ||
		!strings.Contains(stdout, "permitted_python_lookup") || !strings.Contains(stdout, "SAFE_PYTHON_SEARCH_TOKEN") {
		t.Fatalf("permitted search = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func loadSnapshotTaintChunks(t *testing.T, storeDirectory, repository string) []source.Chunk {
	t.Helper()
	store, err := localstore.OpenExisting(storeDirectory)
	if err != nil {
		t.Fatalf("OpenExisting(snapshot) error = %v", err)
	}
	chunks, err := store.Load(context.Background(), canonicalPath(t, repository))
	if err != nil {
		t.Fatalf("Load(snapshot) error = %v", err)
	}
	return chunks
}

func loadSQLiteTaintChunks(t *testing.T, storeDirectory, repository string) ([]source.Chunk, string) {
	t.Helper()
	store, err := indexsqlite.OpenExisting(storeDirectory)
	if err != nil {
		t.Fatalf("OpenExisting(SQLite) error = %v", err)
	}
	repositoryID := canonicalPath(t, repository)
	generationID, err := store.ActiveGeneration(context.Background(), repositoryID)
	if err != nil {
		_ = store.Close()
		t.Fatalf("ActiveGeneration() error = %v", err)
	}
	reader, err := store.BindActive(context.Background(), repositoryID)
	if err != nil {
		_ = store.Close()
		t.Fatalf("BindActive() error = %v", err)
	}
	chunks, err := reader.Load(context.Background(), repositoryID)
	if err != nil {
		_ = reader.Close()
		_ = store.Close()
		t.Fatalf("Load(SQLite) error = %v", err)
	}
	if err := reader.Close(); err != nil {
		_ = store.Close()
		t.Fatalf("Close(reader) error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(SQLite) error = %v", err)
	}
	return chunks, generationID
}

func assertPermittedTaintChunks(t *testing.T, chunks []source.Chunk, forbidden []string) {
	t.Helper()
	if len(chunks) != 2 {
		t.Fatalf("stored chunks = %d, want two permitted chunks", len(chunks))
	}
	assertNoCLITaintValue(t, "stored structured chunks", chunks, forbidden)
	wantReferences := []source.Reference{
		{Path: "safe/allowed.py", StartLine: 1, EndLine: 2},
		{Path: "safe/allowed.ts", StartLine: 1, EndLine: 3},
	}
	for index, chunk := range chunks {
		if chunk.ID == "" || chunk.Reference != wantReferences[index] || !chunk.Reference.Valid() {
			t.Fatalf("chunk %d = %#v, want stable ID and reference %#v", index, chunk, wantReferences[index])
		}
	}
}

func assertTaintHTTPRequestsClean(t *testing.T, requests []taintHTTPRequest, forbidden []string) {
	t.Helper()
	for index, request := range requests {
		if request.Method != http.MethodPost || request.Path != "/v1/embeddings" || request.ReadError != "" {
			t.Fatalf("request %d = %#v, want successful POST capture", index, request)
		}
		host, _, err := net.SplitHostPort(request.Host)
		if err != nil {
			t.Fatalf("request %d host = %q: %v", index, request.Host, err)
		}
		address := net.ParseIP(host)
		if address == nil || !address.IsLoopback() {
			t.Fatalf("request %d host = %q, want numeric loopback", index, request.Host)
		}
		assertNoCLITaintValue(t, fmt.Sprintf("HTTP request %d", index), request, forbidden)
	}
}

func decodeTaintInputs(t *testing.T, body []byte) []string {
	t.Helper()
	var request struct {
		Input []string `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode captured request error = %v", err)
	}
	return request.Input
}

func assertCLITaintSinksClean(t *testing.T, fixture cliTaintFixture, extra []string, values ...string) {
	t.Helper()
	forbidden := appendCopy(fixture.forbidden, canonicalPath(t, fixture.repository))
	forbidden = append(forbidden, extra...)
	assertNoCLITaintValue(t, "CLI stdout/stderr/error sinks", values, forbidden)
}

func assertTaintStoreClean(t *testing.T, storeDirectory string, forbidden []string) {
	t.Helper()
	err := filepath.WalkDir(storeDirectory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("store artifact is not regular: %s", entry.Name())
		}
		if info.Size() > 70<<20 {
			return fmt.Errorf("store artifact exceeds bounded inspection limit: %s", entry.Name())
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(storeDirectory, path)
		if err != nil {
			return err
		}
		checkCLITaintBytes(t, "store artifact name", []byte(relative), forbidden)
		checkCLITaintBytes(t, "store artifact bytes", payload, forbidden)
		return nil
	})
	if err != nil {
		t.Fatalf("inspect store artifacts error = %v", err)
	}
}

func assertNoCLITaintValue(t *testing.T, label string, value any, forbidden []string) {
	t.Helper()
	assertNoCLITaintReflect(t, label, reflect.ValueOf(value), forbidden)
	checkCLITaintBytes(t, label+" debug", []byte(fmt.Sprintf("%#v", value)), forbidden)
	if payload, err := json.Marshal(value); err == nil {
		checkCLITaintBytes(t, label+" JSON", payload, forbidden)
	}
}

func assertNoCLITaintReflect(t *testing.T, label string, value reflect.Value, forbidden []string) {
	t.Helper()
	if !value.IsValid() {
		return
	}
	if value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if !value.IsNil() {
			assertNoCLITaintReflect(t, label, value.Elem(), forbidden)
		}
		return
	}
	if value.Kind() == reflect.String {
		checkCLITaintBytes(t, label+" string", []byte(value.String()), forbidden)
		return
	}
	if value.Kind() == reflect.Slice && value.Type().Elem().Kind() == reflect.Uint8 {
		checkCLITaintBytes(t, label+" bytes", value.Bytes(), forbidden)
		return
	}
	switch value.Kind() {
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			assertNoCLITaintReflect(t, label, value.Field(index), forbidden)
		}
	case reflect.Slice, reflect.Array:
		for index := 0; index < value.Len(); index++ {
			assertNoCLITaintReflect(t, label, value.Index(index), forbidden)
		}
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			assertNoCLITaintReflect(t, label, iterator.Key(), forbidden)
			assertNoCLITaintReflect(t, label, iterator.Value(), forbidden)
		}
	}
}

func checkCLITaintBytes(t *testing.T, label string, payload []byte, forbidden []string) {
	t.Helper()
	for _, canary := range forbidden {
		forms := [][]byte{
			[]byte(canary),
			[]byte(base64.StdEncoding.EncodeToString([]byte(canary))),
			[]byte(fmt.Sprintf("%#v", []byte(canary))),
		}
		for _, form := range forms {
			if len(form) != 0 && bytes.Contains(payload, form) {
				t.Fatalf("%s contains forbidden taint %q", label, canary)
			}
		}
	}
}

func appendCopy(base []string, extra ...string) []string {
	combined := make([]string, 0, len(base)+len(extra))
	combined = append(combined, base...)
	return append(combined, extra...)
}

func writeCLIBytes(t *testing.T, root, relativePath string, content []byte) {
	t.Helper()
	path := filepath.Join(root, relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", relativePath, err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", relativePath, err)
	}
}
