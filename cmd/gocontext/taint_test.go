package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	indexsqlite "github.com/ToledoVitor/GoContext/internal/index/sqlite"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/ingest/filesystem"
	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
	searchdomain "github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/source"
	"github.com/ToledoVitor/GoContext/internal/testsupport/taintcheck"
)

const (
	cliTaintSecurityCanary      = "CLI_TAINT_SECURITY_CANARY_TASK13"
	cliTaintDependencyCanary    = "CLI_TAINT_DEPENDENCY_CANARY_TASK13"
	cliTaintNewDependencyCanary = "CLI_TAINT_NEW_DEPENDENCY_CANARY_TASK14C"
	cliTaintNestedCanary        = "CLI_TAINT_NESTED_CANARY_TASK13"
	cliTaintSymlinkCanary       = "CLI_TAINT_SYMLINK_CANARY_TASK13"
	cliTaintBinaryCanary        = "CLI_TAINT_BINARY_CANARY_TASK13"
	cliTaintTooLargeCanary      = "CLI_TAINT_TOO_LARGE_CANARY_TASK13"
	cliTaintGeneratedCanary     = "CLI_TAINT_GENERATED_CANARY_TASK13"
	cliTaintUnsupportedCanary   = "CLI_TAINT_UNSUPPORTED_CANARY_TASK13"
	cliTaintInvalidUTF8Canary   = "CLI_TAINT_INVALID_UTF8_CANARY_TASK13"
	cliTaintPEMCanary           = "CLI_TAINT_PEM_CONTENT_CANARY_TASK13"
	cliTaintPythonSecretCanary  = "CLI_TAINT_PYTHON_SECRET_CANARY_TASK13"
	cliTaintTypedSecretCanary   = "CLI_TAINT_TYPED_SECRET_CANARY_TASK13"
	cliTaintJavaScriptCanary    = "CLI_TAINT_JAVASCRIPT_EXCLUDED_CANARY_TASK14D"
	cliTaintControlPathCanary   = "CLI_TAINT_CONTROL_PATH_CANARY_TASK13"
	cliTaintProviderKey         = "CLI_TAINT_PROVIDER_KEY_TASK13"
	cliTaintProviderBody        = "CLI_TAINT_PROVIDER_BODY_TASK13"
	cliTaintMalformedBody       = "CLI_TAINT_MALFORMED_BODY_TASK13"
	cliTaintQueryCanary         = "CLI_TAINT_QUERY_EGRESS_CANARY_TASK13"
	cliTaintReplacement         = "CLI_SAFE_UNPUBLISHED_REPLACEMENT_TASK13"
)

type cliTaintFixture struct {
	repository string
	forbidden  []string
}

type taintHTTPRequest struct {
	Method           string
	URLString        string
	Path             string
	RequestURI       string
	RawQuery         string
	Host             string
	Header           http.Header
	Trailer          http.Header
	TransferEncoding []string
	ContentLength    int64
	Body             []byte
	BodyOverflow     bool
	BodyTruncated    bool
	ReadError        string
	CloseError       string
}

const maxTaintHTTPRequestBytes = 1 << 20

type taintHTTPCapture struct {
	mu       sync.Mutex
	requests []taintHTTPRequest
}

type sqliteTaintTable struct {
	Columns []string
	Rows    [][]any
}

type sqliteTaintState struct {
	Schema sqliteTaintTable
	Tables map[string]sqliteTaintTable
}

const (
	maxSQLiteTaintRows       = 128
	maxSQLiteTaintValueBytes = 1 << 20
	maxSQLiteTaintTotalBytes = 8 << 20
)

func TestDecodeStrictTaintEmbeddingRequestRejectsNonExactEnvelope(t *testing.T) {
	valid := `{"model":"task13-model","input":["safe"],"encoding_format":"float","dimensions":2}`
	request, err := decodeStrictTaintEmbeddingRequest([]byte(valid))
	if err != nil || !reflect.DeepEqual(request.Input, []string{"safe"}) {
		t.Fatalf("decode valid request = %#v, %v", request, err)
	}
	invalid := []string{
		`{"model":"task13-model","input":["safe"],"encoding_format":"float","dimensions":2,"extra":"x"}`,
		`{"model":"task13-model","input":["safe"],"input":["safe"],"encoding_format":"float","dimensions":2}`,
		valid + `{}`,
		`{"model":"other","input":["safe"],"encoding_format":"float","dimensions":2}`,
		`{"model":"task13-model","input":["safe"],"encoding_format":"base64","dimensions":2}`,
		`{"model":"task13-model","input":["safe"],"encoding_format":"float","dimensions":3}`,
	}
	for _, payload := range invalid {
		if _, err := decodeStrictTaintEmbeddingRequest([]byte(payload)); err == nil {
			t.Fatalf("decode invalid request %q succeeded", payload)
		}
	}
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
	searchCode, searchOut, searchErr, searchHits := runTaintSearchCommand(
		t, "search", "--store", storeDirectory, "--language", "python", "--language", "typescript",
		fixture.repository, "permitted", "python", "lookup",
	)
	server.Close()

	if indexCode != 0 || indexOut != "indexado: 3 arquivos, 3 símbolos, 3 chunks\n" || indexErr != "" {
		t.Fatalf("snapshot/off index = code %d stdout %q stderr %q", indexCode, indexOut, indexErr)
	}
	assertPermittedSearchOutput(t, searchCode, searchOut, searchErr)
	if requests := capture.snapshot(); len(requests) != 0 {
		t.Fatalf("snapshot/off HTTP requests = %d, want zero", len(requests))
	}

	chunks := loadSnapshotTaintChunks(t, storeDirectory, fixture.repository)
	assertPermittedTaintChunks(t, chunks, fixture.forbidden)
	assertSnapshotTaintArtifact(
		t, storeDirectory, fixture.repository, chunks, appendCopy(fixture.forbidden, cliTaintProviderKey),
	)
	if _, err := os.Lstat(rollbackMarkerPath(storeDirectory, canonicalPath(t, fixture.repository))); !os.IsNotExist(err) {
		t.Fatalf("snapshot/off rollback marker stat error = %v, want no marker", err)
	}
	assertPermittedTaintHits(t, searchHits, chunks, permittedTaintLexicalScores(), fixture.forbidden)
	javaScriptCode, javaScriptOut, javaScriptErr, javaScriptHits := runTaintSearchCommand(
		t, "search", "--store", storeDirectory, "--language", "javascript", fixture.repository, "javascript",
	)
	assertPermittedJavaScriptSearch(t, javaScriptCode, javaScriptOut, javaScriptErr, javaScriptHits, chunks, fixture.forbidden)
	assertCLITaintSinksClean(t, fixture, []string{cliTaintProviderKey}, indexOut, indexErr, searchOut, searchErr, javaScriptOut, javaScriptErr)
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
	searchCode, searchOut, searchErr, searchHits := runTaintSearchCommand(
		t, "search", "--store", storeDirectory, "--index-backend", "auto",
		"--language", "python", "--language", "typescript", fixture.repository, "permitted", "python", "lookup",
	)
	server.Close()

	if indexCode != 0 || indexOut != "indexado: 3 arquivos, 3 símbolos, 3 chunks\n" || indexErr != "" {
		t.Fatalf("SQLite/off index = code %d stdout %q stderr %q", indexCode, indexOut, indexErr)
	}
	assertPermittedSearchOutput(t, searchCode, searchOut, searchErr)
	if requests := capture.snapshot(); len(requests) != 0 {
		t.Fatalf("SQLite/off HTTP requests = %d, want zero", len(requests))
	}

	chunks, generationID := loadSQLiteTaintChunks(t, storeDirectory, fixture.repository)
	assertPermittedTaintChunks(t, chunks, fixture.forbidden)
	structuredForbidden := appendCopy(fixture.forbidden, cliTaintProviderKey)
	state := dumpSQLiteTaintState(t, storeDirectory)
	wantGenerationID := assertSQLiteTaintState(t, state, fixture.repository, generationID, chunks, "", structuredForbidden)
	assertRollbackTaintCompanion(t, storeDirectory, fixture.repository, wantGenerationID, chunks, structuredForbidden)
	assertPermittedTaintHits(t, searchHits, chunks, permittedTaintLexicalScores(), fixture.forbidden)
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
	searchArgs = append(searchArgs, "--language", "python", "--language", "typescript", fixture.repository, "permitted", "python", "lookup")
	searchCode, searchOut, searchErr, searchHits := runTaintSearchCommand(t, searchArgs...)
	server.Close()

	if indexCode != 0 || indexOut != "indexado: 3 arquivos, 3 símbolos, 3 chunks\nsemântica: status=degraded vetores=0 requests=3 tokens=0\n" ||
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
	wantDocumentInputs := permittedTaintDocumentInputs()
	assertExactTaintHTTPRequests(
		t, requests, fixture,
		[][]string{wantDocumentInputs, wantDocumentInputs, wantDocumentInputs}, -1,
	)

	chunks, generationID := loadSQLiteTaintChunks(t, storeDirectory, fixture.repository)
	assertPermittedTaintChunks(t, chunks, fixture.forbidden)
	extra := []string{cliTaintProviderKey, cliTaintProviderBody}
	state := dumpSQLiteTaintState(t, storeDirectory)
	wantGenerationID := assertSQLiteTaintState(t, state, fixture.repository, generationID, chunks, "", appendCopy(fixture.forbidden, extra...))
	assertRollbackTaintCompanion(t, storeDirectory, fixture.repository, wantGenerationID, chunks, appendCopy(fixture.forbidden, extra...))
	assertPermittedTaintHits(t, searchHits, chunks, permittedTaintLexicalScores(), fixture.forbidden)
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
	searchArgs = append(searchArgs, "--language", "python", "--language", "typescript", fixture.repository, cliTaintQueryCanary)
	searchCode, searchOut, searchErr, searchHits := runTaintSearchCommand(t, searchArgs...)
	server.Close()

	if indexCode != 0 || indexOut != "indexado: 3 arquivos, 3 símbolos, 3 chunks\nsemântica: status=indexed vetores=3 requests=1 tokens=7\n" || indexErr != "" {
		t.Fatalf("SQLite/required index = code %d stdout %q stderr %q", indexCode, indexOut, indexErr)
	}
	if searchCode != 0 || searchErr != "" || !strings.Contains(searchOut, "safe/allowed.py:1-2") {
		t.Fatalf("SQLite/required search = code %d stdout %q stderr %q", searchCode, searchOut, searchErr)
	}

	requests := capture.snapshot()
	if len(requests) != 2 {
		t.Fatalf("SQLite/required requests = %d, want one document and one query request", len(requests))
	}
	assertExactTaintHTTPRequests(
		t, requests, fixture,
		[][]string{permittedTaintDocumentInputs(), {cliTaintQueryCanary}}, 1,
	)
	chunks, generationID := loadSQLiteTaintChunks(t, storeDirectory, fixture.repository)
	assertPermittedTaintChunks(t, chunks, fixture.forbidden)
	extra := []string{cliTaintProviderKey, cliTaintQueryCanary}
	state := dumpSQLiteTaintState(t, storeDirectory)
	wantGenerationID := assertSQLiteTaintState(t, state, fixture.repository, generationID, chunks, server.URL+"/v1/embeddings", appendCopy(fixture.forbidden, extra...))
	assertRollbackTaintCompanion(t, storeDirectory, fixture.repository, wantGenerationID, chunks, appendCopy(fixture.forbidden, extra...))
	assertPermittedTaintHits(t, searchHits, chunks, []float64{0.5, 0.4919354838709677}, fixture.forbidden)
	assertCLITaintSinksClean(t, fixture, extra, indexOut, indexErr, searchOut, searchErr)
	assertTaintStoreClean(t, storeDirectory, appendCopy(fixture.forbidden, cliTaintProviderKey, cliTaintQueryCanary))
}

func TestRunSQLiteRequiredRetryThenMalformedResponsePreservesPriorGeneration(t *testing.T) {
	clearEmbeddingEnvironment(t)
	fixture := newCLITaintFixture(t)
	storeDirectory := t.TempDir()
	extra := []string{cliTaintProviderKey, cliTaintProviderBody, cliTaintMalformedBody, cliTaintReplacement}
	structuredForbidden := appendCopy(fixture.forbidden, extra...)
	baselineServer, baselineCapture := newNumericLoopbackTaintServer(t, func(writer http.ResponseWriter, request taintHTTPRequest, _ int) {
		writeValidTaintEmbeddings(writer, request.Body)
	})
	t.Setenv(embeddingAPIKeyEnv, cliTaintProviderKey)
	baselineArgs := append(
		[]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", "--semantic", "required"},
		taintProviderFlags(baselineServer.URL)...,
	)
	baselineArgs = append(baselineArgs, fixture.repository)

	baselineCode, baselineOut, baselineErr := runTaintCommand(t, baselineArgs...)
	baselineServer.Close()
	if baselineCode != 0 || baselineOut != "indexado: 3 arquivos, 3 símbolos, 3 chunks\nsemântica: status=indexed vetores=3 requests=1 tokens=7\n" || baselineErr != "" {
		t.Fatalf("required baseline = code %d stdout %q stderr %q", baselineCode, baselineOut, baselineErr)
	}
	baselineRequests := baselineCapture.snapshot()
	if len(baselineRequests) != 1 {
		t.Fatalf("required baseline requests = %d, want one", len(baselineRequests))
	}
	assertExactTaintHTTPRequests(t, baselineRequests, fixture, [][]string{permittedTaintDocumentInputs()}, -1)
	baselineChunks, baselineGeneration := loadSQLiteTaintChunks(t, storeDirectory, fixture.repository)
	assertPermittedTaintChunks(t, baselineChunks, fixture.forbidden)
	baselineState := dumpSQLiteTaintState(t, storeDirectory)
	baselineExpectedGeneration := assertSQLiteTaintState(
		t, baselineState, fixture.repository, baselineGeneration, baselineChunks,
		baselineServer.URL+"/v1/embeddings", structuredForbidden,
	)
	companionBefore := assertRollbackTaintCompanion(
		t, storeDirectory, fixture.repository, baselineExpectedGeneration, baselineChunks, structuredForbidden,
	)

	writeCLIFile(t, fixture.repository, "safe/allowed.py", "def unpublished_replacement():\n    return \""+cliTaintReplacement+"\"\n")
	var failureAttempts atomic.Int64
	failureServer, failureCapture := newNumericLoopbackTaintServer(t, func(writer http.ResponseWriter, request taintHTTPRequest, _ int) {
		attempt := failureAttempts.Add(1)
		if attempt < 3 {
			writer.Header().Set("Retry-After", "0")
			http.Error(writer, cliTaintProviderBody, http.StatusTooManyRequests)
			return
		}
		writeMalformedTaintEmbeddings(writer, request.Body)
	})
	failureArgs := append(
		[]string{"index", "--store", storeDirectory, "--index-backend", "sqlite", "--semantic", "required"},
		taintProviderFlags(failureServer.URL)...,
	)
	failureArgs = append(failureArgs, fixture.repository)
	failureCode, failureOut, failureErr := runTaintCommand(t, failureArgs...)
	failureServer.Close()

	if failureCode != 1 || failureOut != "" || failureErr != "indexar repositório: falha na indexação SQLite\n" {
		t.Fatalf("required retry/malformed = code %d stdout %q stderr %q", failureCode, failureOut, failureErr)
	}
	afterChunks, afterGeneration := loadSQLiteTaintChunks(t, storeDirectory, fixture.repository)
	if afterGeneration != baselineGeneration || !reflect.DeepEqual(afterChunks, baselineChunks) {
		t.Fatalf("required failure generation/chunks changed: generation %q -> %q", baselineGeneration, afterGeneration)
	}
	afterState := dumpSQLiteTaintState(t, storeDirectory)
	afterExpectedGeneration := assertSQLiteTaintState(
		t, afterState, fixture.repository, afterGeneration, afterChunks,
		baselineServer.URL+"/v1/embeddings", structuredForbidden,
	)
	if afterExpectedGeneration != baselineExpectedGeneration {
		t.Fatalf("independent generation identity changed: %q -> %q", baselineExpectedGeneration, afterExpectedGeneration)
	}
	if !reflect.DeepEqual(afterState, baselineState) {
		t.Fatalf("required failure changed complete structured SQLite state:\nbefore = %#v\nafter = %#v", baselineState, afterState)
	}
	companionAfter := assertRollbackTaintCompanion(
		t, storeDirectory, fixture.repository, afterExpectedGeneration, afterChunks, structuredForbidden,
	)
	if !bytes.Equal(companionAfter.SnapshotBytes, companionBefore.SnapshotBytes) ||
		!bytes.Equal(companionAfter.MarkerBytes, companionBefore.MarkerBytes) {
		t.Fatal("required retry/malformed failure changed exact rollback companion bytes")
	}

	failureRequests := failureCapture.snapshot()
	if len(failureRequests) != 3 || failureAttempts.Load() != 3 {
		t.Fatalf("required failure requests = %d with %d attempts, want three captured attempts", len(failureRequests), failureAttempts.Load())
	}
	permittedInputs := permittedTaintDocumentInputs()
	wantFailureInputs := []string{
		permittedInputs[0],
		"def unpublished_replacement():\n    return \"" + cliTaintReplacement + "\"",
		permittedInputs[2],
	}
	assertExactTaintHTTPRequests(
		t, failureRequests, fixture,
		[][]string{wantFailureInputs, wantFailureInputs, wantFailureInputs}, -1,
	)

	rollbackCode, rollbackOut, rollbackErr, rollbackHits := runTaintSearchCommand(
		t, "search", "--store", storeDirectory, "--index-backend", "snapshot",
		"--language", "python", "--language", "typescript", fixture.repository, "permitted", "python", "lookup",
	)
	assertPermittedSearchOutput(t, rollbackCode, rollbackOut, rollbackErr)
	assertPermittedTaintHits(t, rollbackHits, baselineChunks, permittedTaintLexicalScores(), structuredForbidden)
	companionAfterRollback := assertRollbackTaintCompanion(
		t, storeDirectory, fixture.repository, baselineExpectedGeneration, baselineChunks, structuredForbidden,
	)
	if !bytes.Equal(companionAfterRollback.SnapshotBytes, companionBefore.SnapshotBytes) ||
		!bytes.Equal(companionAfterRollback.MarkerBytes, companionBefore.MarkerBytes) {
		t.Fatal("explicit rollback search changed exact rollback companion bytes")
	}
	postRollbackState := dumpSQLiteTaintState(t, storeDirectory)
	if !reflect.DeepEqual(postRollbackState, baselineState) {
		t.Fatal("explicit rollback search changed complete structured SQLite state")
	}
	assertCLITaintSinksClean(
		t, fixture, extra, baselineOut, baselineErr, failureOut, failureErr, rollbackOut, rollbackErr,
	)
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
	writeCLIFile(t, repository, "safe/allowed.jsx", "export const safeJavaScriptEntry = () => \"SAFE_JAVASCRIPT_SEARCH_TOKEN\"\n")

	for _, path := range []string{
		".env", ".env.local", ".env.jsx", ".git/config", ".github/workflows/ci.js",
		"credentials.js", "secret.jsx", "material.pem", "certificate.crt",
	} {
		writeCLIFile(t, repository, path, cliTaintSecurityCanary+cliTaintJavaScriptCanary+"\n")
	}
	for _, path := range []string{
		"node_modules/dependency.js", "vendor/vendor.jsx", "build/output.js", ".cache/cache.jsx",
	} {
		writeCLIFile(t, repository, path, cliTaintDependencyCanary+cliTaintJavaScriptCanary+"\n")
	}
	newDependencyPaths := []string{
		"Pods/dependency.js", ".gradle/dependency.jsx", ".dart_tool/dependency.js", ".pub-cache/dependency.jsx",
		"DerivedData/dependency.js", "Carthage/dependency.jsx", ".cxx/dependency.js", ".expo/dependency.jsx",
		".turbo/dependency.js", ".nx/dependency.jsx", ".parcel-cache/dependency.js", ".vite/dependency.jsx",
		".bundle/dependency.js",
	}
	for index, path := range newDependencyPaths {
		writeCLIFile(t, repository, path, fmt.Sprintf("%s_%02d\n", cliTaintNewDependencyCanary, index))
	}
	writeCLIFile(t, repository, "nested-repository/.git/config", cliTaintNestedCanary+"\n")
	writeCLIFile(t, repository, "nested-repository/00-child.js", "export function "+cliTaintNestedCanary+"() { return '"+cliTaintJavaScriptCanary+"' }\n")
	writeCLIFile(t, repository, "client.generated.js", cliTaintGeneratedCanary+cliTaintJavaScriptCanary+"\n")
	writeCLIFile(t, repository, "generated-header.jsx", "// Code generated for "+cliTaintGeneratedCanary+cliTaintJavaScriptCanary+". DO NOT EDIT.\nconst value = 1\n")
	writeCLIBytes(t, repository, "binary.js", append([]byte(cliTaintBinaryCanary+cliTaintJavaScriptCanary), 0, 'x'))
	oversized := append([]byte(cliTaintTooLargeCanary+cliTaintJavaScriptCanary), bytes.Repeat([]byte{'x'}, int(filesystem.DefaultMaxFileSize)+1)...)
	writeCLIBytes(t, repository, "oversized.jsx", oversized)
	writeCLIBytes(t, repository, "invalid-utf8.js", append([]byte(cliTaintInvalidUTF8Canary+cliTaintJavaScriptCanary), 0xff))
	writeCLIFile(t, repository, "opaque.CLI_TAINT_UNSUPPORTED_NAME_TASK13", cliTaintUnsupportedCanary+"\n")
	writeCLIFile(t, repository, "pem-material.py", "MATERIAL = '''-----BEGIN PRIVATE KEY-----\n"+cliTaintPEMCanary+"\n-----END PRIVATE KEY-----'''\n")
	writeCLIFile(t, repository, "embedded-secret.py", "safe = \"x\"; password: str = \""+cliTaintPythonSecretCanary+"\"\n")
	writeCLIFile(t, repository, "embedded-secret.jsx", "const safe = \"x\"; const token = \""+cliTaintTypedSecretCanary+cliTaintJavaScriptCanary+"\"\n")

	forbidden := []string{
		cliTaintSecurityCanary, cliTaintDependencyCanary, cliTaintNewDependencyCanary, cliTaintNestedCanary, cliTaintBinaryCanary,
		cliTaintTooLargeCanary, cliTaintGeneratedCanary, cliTaintUnsupportedCanary, cliTaintInvalidUTF8Canary,
		cliTaintPEMCanary, cliTaintPythonSecretCanary, cliTaintTypedSecretCanary, cliTaintJavaScriptCanary,
		".env", ".env.local", ".env.jsx", ".git/config", ".github/workflows/ci.js",
		"credentials.js", "secret.jsx", "material.pem", "certificate.crt",
		"node_modules/dependency.js", "vendor/vendor.jsx", "build/output.js", ".cache/cache.jsx",
		"nested-repository/.git/config", "nested-repository/00-child.js", "client.generated.js",
		"generated-header.jsx", "binary.js", "oversized.jsx", "invalid-utf8.js",
		"opaque.CLI_TAINT_UNSUPPORTED_NAME_TASK13", "pem-material.py", "embedded-secret.py", "embedded-secret.jsx",
	}
	forbidden = append(forbidden, newDependencyPaths...)
	if runtime.GOOS != "windows" {
		writeCLIFile(t, outside, "outside.jsx", cliTaintSymlinkCanary+cliTaintJavaScriptCanary+"\n")
		writeCLIFile(t, repository, ".env.internal-link", cliTaintSymlinkCanary+"\n")
		if err := os.Symlink(filepath.Join(outside, "outside.jsx"), filepath.Join(repository, "external-link.jsx")); err != nil {
			t.Fatalf("Symlink(external) error = %v", err)
		}
		if err := os.Symlink(".env.internal-link", filepath.Join(repository, "internal-link.js")); err != nil {
			t.Fatalf("Symlink(internal) error = %v", err)
		}
		forbidden = append(
			forbidden,
			cliTaintSymlinkCanary,
			".env.internal-link",
			"external-link.jsx",
			"internal-link.js",
			filepath.Join(outside, "outside.jsx"),
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
		body, readErr := io.ReadAll(io.LimitReader(request.Body, maxTaintHTTPRequestBytes+1))
		closeErr := request.Body.Close()
		record := taintHTTPRequest{
			Method: request.Method, URLString: request.URL.String(), Path: request.URL.Path,
			RequestURI: request.RequestURI, RawQuery: request.URL.RawQuery, Host: request.Host,
			Header: request.Header.Clone(), Trailer: request.Trailer.Clone(),
			TransferEncoding: append([]string(nil), request.TransferEncoding...),
			ContentLength:    request.ContentLength, Body: append([]byte(nil), body...),
			BodyOverflow:  len(body) > maxTaintHTTPRequestBytes,
			BodyTruncated: request.ContentLength >= 0 && int64(len(body)) != request.ContentLength,
		}
		if readErr != nil {
			record.ReadError = readErr.Error()
		}
		if closeErr != nil {
			record.CloseError = closeErr.Error()
		}
		ordinal := capture.add(record)
		if record.BodyOverflow || record.BodyTruncated || record.ReadError != "" || record.CloseError != "" {
			http.Error(writer, "invalid bounded synthetic request", http.StatusRequestEntityTooLarge)
			return
		}
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
		requests[index].Trailer = request.Trailer.Clone()
		requests[index].TransferEncoding = append([]string(nil), request.TransferEncoding...)
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
	request, err := decodeStrictTaintEmbeddingRequest(body)
	if err != nil {
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

func runTaintSearchCommand(t *testing.T, args ...string) (int, string, string, []searchdomain.Hit) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var hits []searchdomain.Hit
	code := runWithSearchObserver(args, &stdout, &stderr, func(observed []searchdomain.Hit) {
		hits = append([]searchdomain.Hit(nil), observed...)
	})
	return code, stdout.String(), stderr.String(), hits
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

type taintSnapshot struct {
	Version        int            `json:"version"`
	RepositoryID   string         `json:"repository_id"`
	PolicyVersion  string         `json:"policy_version"`
	CorpusRevision string         `json:"corpus_revision"`
	Chunks         []source.Chunk `json:"chunks"`
}

type taintSnapshotArtifact struct {
	Bytes  []byte
	Corpus source.Corpus
}

type taintRollbackCompanion struct {
	SnapshotBytes []byte
	MarkerBytes   []byte
}

func assertSnapshotTaintArtifact(
	t *testing.T,
	storeDirectory, repository string,
	wantChunks []source.Chunk,
	forbidden []string,
) taintSnapshotArtifact {
	t.Helper()
	repositoryID := canonicalPath(t, repository)
	path := filepath.Join(storeDirectory, repositoryHash(repositoryID)+".json")
	payload := readPrivateRegularTaintArtifact(t, path, 64<<20)
	checkCLITaintBytes(t, "strict snapshot payload", payload, forbidden)
	if !hasExactTaintJSONFields(payload, []string{"version", "repository_id", "policy_version", "corpus_revision", "chunks"}) {
		t.Fatalf("snapshot %q does not contain exactly the v2 fields", filepath.Base(path))
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var stored taintSnapshot
	if err := decoder.Decode(&stored); err != nil {
		t.Fatalf("decode strict snapshot error = %v", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("decode strict snapshot trailing data error = %v", err)
	}
	if stored.Version != 2 || stored.RepositoryID != repositoryID ||
		stored.PolicyVersion != ingest.ScanPolicyVersion || !reflect.DeepEqual(stored.Chunks, wantChunks) {
		t.Fatalf("strict snapshot = %#v, want exact current permitted corpus", stored)
	}
	corpus, err := source.NewCorpus(stored.PolicyVersion, stored.Chunks)
	if err != nil || corpus.Revision != stored.CorpusRevision {
		t.Fatalf("validate strict snapshot corpus = %#v, error %v", corpus, err)
	}
	loaded := loadSnapshotTaintChunks(t, storeDirectory, repository)
	if !reflect.DeepEqual(loaded, wantChunks) {
		t.Fatalf("strict snapshot Load() = %#v, want exact current permitted chunks", loaded)
	}
	return taintSnapshotArtifact{Bytes: payload, Corpus: corpus}
}

func assertRollbackTaintCompanion(
	t *testing.T,
	storeDirectory, repository, generationID string,
	wantChunks []source.Chunk,
	forbidden []string,
) taintRollbackCompanion {
	t.Helper()
	repositoryID := canonicalPath(t, repository)
	snapshot := assertSnapshotTaintArtifact(t, storeDirectory, repository, wantChunks, forbidden)
	path := rollbackMarkerPath(storeDirectory, repositoryID)
	payload := readPrivateRegularTaintArtifact(t, path, maxRollbackMarkerSize)
	checkCLITaintBytes(t, "strict rollback marker payload", payload, forbidden)
	if !hasExactRollbackMarkerFields(payload) {
		t.Fatalf("rollback marker %q does not contain exactly the required fields", filepath.Base(path))
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var marker rollbackMarker
	if err := decoder.Decode(&marker); err != nil {
		t.Fatalf("decode strict rollback marker error = %v", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("decode strict rollback marker trailing data error = %v", err)
	}
	want := rollbackMarker{
		Version:          rollbackMarkerSchemaVersion,
		RepositoryHash:   repositoryHash(repositoryID),
		ScanPolicy:       ingest.ScanPolicyVersion,
		CorpusRevision:   snapshot.Corpus.Revision,
		ActiveGeneration: generationID,
	}
	if marker != want {
		t.Fatalf("rollback marker = %#v, want exact %#v", marker, want)
	}
	if runtime.GOOS != "windows" {
		validated, err := readRollbackMarker(context.Background(), storeDirectory, repositoryID)
		if err != nil || validated != want {
			t.Fatalf("readRollbackMarker() = %#v, %v; want exact %#v", validated, err, want)
		}
	}
	return taintRollbackCompanion{SnapshotBytes: snapshot.Bytes, MarkerBytes: payload}
}

func readPrivateRegularTaintArtifact(t *testing.T, path string, maxBytes int64) []byte {
	t.Helper()
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%q) error = %v", filepath.Base(path), err)
	}
	assertPrivateRegularTaintMode(t, path, "before", before)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open(%q) error = %v", filepath.Base(path), err)
	}
	opened, statErr := file.Stat()
	payload, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	after, pathErr := os.Lstat(path)
	if statErr != nil || readErr != nil || closeErr != nil || pathErr != nil {
		t.Fatalf("inspect artifact %q errors = stat %v read %v close %v path %v", filepath.Base(path), statErr, readErr, closeErr, pathErr)
	}
	assertPrivateRegularTaintMode(t, path, "opened", opened)
	assertPrivateRegularTaintMode(t, path, "after", after)
	if !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		t.Fatalf("artifact %q changed during inspection", filepath.Base(path))
	}
	if int64(len(payload)) > maxBytes {
		t.Fatalf("artifact %q exceeds %d-byte inspection bound", filepath.Base(path), maxBytes)
	}
	return payload
}

func assertPrivateRegularTaintMode(t *testing.T, path, stage string, info os.FileInfo) {
	t.Helper()
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Fatalf("artifact %q %s mode = %v, want regular non-symlink", filepath.Base(path), stage, info)
		return
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("artifact %q %s permissions = %o, want private", filepath.Base(path), stage, info.Mode().Perm())
	}
}

func hasExactTaintJSONFields(payload []byte, fieldNames []string) bool {
	expected := make(map[string]struct{}, len(fieldNames))
	for _, fieldName := range fieldNames {
		expected[fieldName] = struct{}{}
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := make(map[string]struct{}, len(expected))
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, isString := keyToken.(string)
		if err != nil || !isString {
			return false
		}
		if _, allowed := expected[key]; !allowed {
			return false
		}
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return false
		}
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') || len(seen) != len(expected) {
		return false
	}
	return decoder.Decode(&struct{}{}) == io.EOF
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

func dumpSQLiteTaintState(t *testing.T, storeDirectory string) sqliteTaintState {
	t.Helper()
	databasePath := filepath.Join(storeDirectory, "index-v2.sqlite3")
	dsn := &url.URL{Scheme: "file", Path: databasePath, RawQuery: "mode=ro"}
	database, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		t.Fatalf("Open(read-only SQLite dump) error = %v", err)
	}
	database.SetMaxOpenConns(1)
	if err := database.PingContext(context.Background()); err != nil {
		_ = database.Close()
		t.Fatalf("Ping(read-only SQLite dump) error = %v", err)
	}

	queries := []struct {
		table string
		query string
	}{
		{table: "sqlite_schema", query: `SELECT * FROM sqlite_schema ORDER BY type, name, tbl_name, rootpage`},
		{table: "schema_version", query: `SELECT * FROM schema_version ORDER BY version`},
		{table: "repositories", query: `SELECT * FROM repositories ORDER BY repository_id`},
		{table: "generations", query: `SELECT * FROM generations ORDER BY repository_id, generation_id`},
		{table: "chunks", query: `SELECT * FROM chunks ORDER BY repository_id, generation_id, ordinal, chunk_id`},
		{table: "vectors", query: `SELECT * FROM vectors ORDER BY repository_id, generation_id, chunk_id`},
	}
	state := sqliteTaintState{Tables: make(map[string]sqliteTaintTable, len(queries)-1)}
	totalBytes := 0
	for _, tableQuery := range queries {
		rows, err := database.QueryContext(context.Background(), tableQuery.query)
		if err != nil {
			_ = database.Close()
			t.Fatalf("Query(%s) error = %v", tableQuery.table, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			_ = database.Close()
			t.Fatalf("Columns(%s) error = %v", tableQuery.table, err)
		}
		table := sqliteTaintTable{Columns: append([]string(nil), columns...)}
		for rows.Next() {
			if len(table.Rows) >= maxSQLiteTaintRows {
				_ = rows.Close()
				_ = database.Close()
				t.Fatalf("table %s exceeds %d-row inspection bound", tableQuery.table, maxSQLiteTaintRows)
			}
			values := make([]any, len(columns))
			destinations := make([]any, len(columns))
			for index := range values {
				destinations[index] = &values[index]
			}
			if err := rows.Scan(destinations...); err != nil {
				_ = rows.Close()
				_ = database.Close()
				t.Fatalf("Scan(%s) error = %v", tableQuery.table, err)
			}
			for index, value := range values {
				switch typed := value.(type) {
				case nil, int64, float64, bool:
				case string:
					if len(typed) > maxSQLiteTaintValueBytes {
						t.Fatalf("table %s text column %s exceeds inspection bound", tableQuery.table, columns[index])
					}
					totalBytes += len(typed)
				case []byte:
					if len(typed) > maxSQLiteTaintValueBytes {
						t.Fatalf("table %s blob column %s exceeds inspection bound", tableQuery.table, columns[index])
					}
					totalBytes += len(typed)
					values[index] = append([]byte(nil), typed...)
				default:
					t.Fatalf("table %s column %s has unexpected SQLite value type %T", tableQuery.table, columns[index], value)
				}
				if totalBytes > maxSQLiteTaintTotalBytes {
					t.Fatalf("SQLite structured dump exceeds %d-byte inspection bound", maxSQLiteTaintTotalBytes)
				}
			}
			table.Rows = append(table.Rows, values)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil || closeErr != nil {
			_ = database.Close()
			t.Fatalf("finish rows(%s) errors = %v, %v", tableQuery.table, rowsErr, closeErr)
		}
		if tableQuery.table == "sqlite_schema" {
			state.Schema = table
		} else {
			state.Tables[tableQuery.table] = table
		}
	}
	if err := database.Close(); err != nil {
		t.Fatalf("Close(read-only SQLite dump) error = %v", err)
	}
	return state
}

func assertSQLiteTaintState(
	t *testing.T,
	state sqliteTaintState,
	repository, generationID string,
	chunks []source.Chunk,
	profileEndpoint string,
	forbidden []string,
) string {
	t.Helper()
	assertNoCLITaintValue(t, "complete structured SQLite state", state, forbidden)
	assertSQLiteTaintSchema(t, state.Schema)
	wantColumns := map[string][]string{
		"schema_version": {"version"},
		"repositories":   {"repository_id", "active_generation"},
		"generations": {
			"repository_id", "generation_id", "corpus_revision", "content_digest", "scan_policy_version",
			"profile_fingerprint", "profile_model", "dimensions", "metric", "vector_digest", "manifest_digest",
		},
		"chunks": {
			"repository_id", "generation_id", "chunk_id", "ordinal", "text", "language", "symbol_name",
			"path", "start_line", "end_line",
		},
		"vectors": {"repository_id", "generation_id", "chunk_id", "encoding_version", "dimensions", "values_blob"},
	}
	if len(state.Tables) != len(wantColumns) {
		t.Fatalf("SQLite tables = %#v, want every complete application table", state.Tables)
	}
	for tableName, columns := range wantColumns {
		table, present := state.Tables[tableName]
		if !present || !reflect.DeepEqual(table.Columns, columns) {
			t.Fatalf("SQLite table %s columns = %#v, want %#v", tableName, table.Columns, columns)
		}
	}
	versionTable := state.Tables["schema_version"]
	if !reflect.DeepEqual(versionTable.Rows, [][]any{{int64(2)}}) {
		t.Fatalf("schema_version rows = %#v, want exact v2 singleton", versionTable.Rows)
	}

	repositoryID := canonicalPath(t, repository)
	corpus, err := source.NewCorpus(ingest.ScanPolicyVersion, chunks)
	if err != nil {
		t.Fatalf("NewCorpus(SQLite state) error = %v", err)
	}
	semantic := profileEndpoint != ""
	var profileFingerprint, profileModel any
	dimensions := int64(0)
	if semantic {
		profileFingerprint = expectedTaintProfileFingerprint(profileEndpoint)
		profileModel = "task13-model"
		dimensions = 2
	}
	wantGenerationID := expectedTaintGenerationID(
		ingest.ScanPolicyVersion, corpus.Revision, semantic, taintStringValue(profileFingerprint),
	)
	if generationID != wantGenerationID {
		t.Fatalf("observed generation ID = %q, want independently derived %q", generationID, wantGenerationID)
	}
	repositories := state.Tables["repositories"]
	if len(repositories.Rows) != 2 {
		t.Fatalf("repository rows = %#v, want identity plus one indexed repository", repositories.Rows)
	}
	var indexedRepositoryRows, identityRows int
	for _, row := range repositories.Rows {
		repositoryValue, ok := row[0].(string)
		if !ok {
			t.Fatalf("repository row = %#v, want text repository ID", row)
		}
		switch {
		case repositoryValue == repositoryID:
			indexedRepositoryRows++
			if row[1] != wantGenerationID {
				t.Fatalf("active generation = %#v, want %q", row[1], wantGenerationID)
			}
		case strings.HasPrefix(repositoryValue, "gocontext:store-identity:v1:"):
			identityRows++
			if row[1] != nil {
				t.Fatalf("identity row active generation = %#v, want NULL", row[1])
			}
		default:
			t.Fatalf("unexpected repository row = %#v", row)
		}
	}
	if indexedRepositoryRows != 1 || identityRows != 1 {
		t.Fatalf("repository row classes = indexed %d identity %d, want one each", indexedRepositoryRows, identityRows)
	}

	generations := state.Tables["generations"]
	if len(generations.Rows) != 1 {
		t.Fatalf("generation rows = %#v, want exactly one active generation and no inactive/orphan state", generations.Rows)
	}
	generation := sqliteRowMap(generations, generations.Rows[0])
	contentDigest := expectedTaintContentDigest(chunks)
	vectorDigest := expectedTaintVectorDigest(chunks, semantic)
	manifestDigest := expectedTaintManifestDigest(
		repositoryID, wantGenerationID, corpus.Revision, contentDigest, ingest.ScanPolicyVersion,
		taintStringValue(profileFingerprint), taintStringValue(profileModel), int(dimensions), vectorDigest,
	)
	wantGeneration := map[string]any{
		"repository_id": repositoryID, "generation_id": wantGenerationID,
		"corpus_revision": corpus.Revision, "content_digest": contentDigest,
		"scan_policy_version": ingest.ScanPolicyVersion,
		"profile_fingerprint": profileFingerprint, "profile_model": profileModel,
		"dimensions": dimensions, "metric": "cosine",
		"vector_digest": vectorDigest, "manifest_digest": manifestDigest,
	}
	if !reflect.DeepEqual(generation, wantGeneration) {
		t.Fatalf("generation row = %#v, want every column exact %#v", generation, wantGeneration)
	}

	chunkTable := state.Tables["chunks"]
	if len(chunkTable.Rows) != len(chunks) {
		t.Fatalf("chunk rows = %d, want %d with no orphan/partial rows", len(chunkTable.Rows), len(chunks))
	}
	for ordinal, row := range chunkTable.Rows {
		stored := sqliteRowMap(chunkTable, row)
		chunk := chunks[ordinal]
		want := map[string]any{
			"repository_id": repositoryID, "generation_id": wantGenerationID, "chunk_id": chunk.ID,
			"ordinal": int64(ordinal), "text": chunk.Text, "language": string(chunk.Language),
			"symbol_name": chunk.SymbolName, "path": chunk.Reference.Path,
			"start_line": int64(chunk.Reference.StartLine), "end_line": int64(chunk.Reference.EndLine),
		}
		if !reflect.DeepEqual(stored, want) {
			t.Fatalf("chunk row %d = %#v, want exact %#v", ordinal, stored, want)
		}
	}

	vectors := state.Tables["vectors"]
	wantVectorCount := 0
	if semantic {
		wantVectorCount = len(chunks)
	}
	if len(vectors.Rows) != wantVectorCount {
		t.Fatalf("vector rows = %#v, want %d complete rows", vectors.Rows, wantVectorCount)
	}
	for _, row := range vectors.Rows {
		stored := sqliteRowMap(vectors, row)
		chunkID, ok := stored["chunk_id"].(string)
		if !ok {
			t.Fatalf("vector row = %#v, want text chunk ID", stored)
		}
		expectedBlob, present := expectedTaintVectorBlob(chunks, chunkID)
		if !present {
			t.Fatalf("unexpected vector chunk ID = %q", chunkID)
		}
		want := map[string]any{
			"repository_id": repositoryID, "generation_id": wantGenerationID, "chunk_id": chunkID,
			"encoding_version": int64(1), "dimensions": int64(2), "values_blob": expectedBlob,
		}
		if !reflect.DeepEqual(stored, want) {
			t.Fatalf("vector row = %#v, want exact %#v", stored, want)
		}
	}
	return wantGenerationID
}

func assertSQLiteTaintSchema(t *testing.T, schema sqliteTaintTable) {
	t.Helper()
	wantColumns := []string{"type", "name", "tbl_name", "rootpage", "sql"}
	if !reflect.DeepEqual(schema.Columns, wantColumns) {
		t.Fatalf("sqlite_schema columns = %#v, want %#v", schema.Columns, wantColumns)
	}
	type object struct {
		kind, table string
		automatic   bool
	}
	want := map[string]object{
		"schema_version":                  {kind: "table", table: "schema_version"},
		"repositories":                    {kind: "table", table: "repositories"},
		"generations":                     {kind: "table", table: "generations"},
		"chunks":                          {kind: "table", table: "chunks"},
		"vectors":                         {kind: "table", table: "vectors"},
		"sqlite_autoindex_repositories_1": {kind: "index", table: "repositories", automatic: true},
		"sqlite_autoindex_generations_1":  {kind: "index", table: "generations", automatic: true},
		"sqlite_autoindex_chunks_1":       {kind: "index", table: "chunks", automatic: true},
		"sqlite_autoindex_chunks_2":       {kind: "index", table: "chunks", automatic: true},
		"sqlite_autoindex_vectors_1":      {kind: "index", table: "vectors", automatic: true},
	}
	if len(schema.Rows) != len(want) {
		t.Fatalf("sqlite_schema objects = %#v, want exact tables and automatic indexes", schema.Rows)
	}
	seen := make(map[string]struct{}, len(schema.Rows))
	for _, row := range schema.Rows {
		stored := sqliteRowMap(schema, row)
		name, nameOK := stored["name"].(string)
		kind, kindOK := stored["type"].(string)
		table, tableOK := stored["tbl_name"].(string)
		rootPage, rootOK := stored["rootpage"].(int64)
		expected, allowed := want[name]
		if !nameOK || !kindOK || !tableOK || !rootOK || rootPage <= 0 || !allowed ||
			kind != expected.kind || table != expected.table {
			t.Fatalf("unexpected sqlite_schema object = %#v", stored)
		}
		if _, duplicate := seen[name]; duplicate {
			t.Fatalf("duplicate sqlite_schema object %q", name)
		}
		seen[name] = struct{}{}
		if expected.automatic {
			if stored["sql"] != nil {
				t.Fatalf("automatic index %q SQL = %#v, want NULL", name, stored["sql"])
			}
			continue
		}
		sqlText, ok := stored["sql"].(string)
		if !ok || !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sqlText)), "CREATE TABLE") {
			t.Fatalf("table %q SQL = %#v, want explicit CREATE TABLE", name, stored["sql"])
		}
	}
}

func sqliteRowMap(table sqliteTaintTable, row []any) map[string]any {
	values := make(map[string]any, len(table.Columns))
	for index, column := range table.Columns {
		values[column] = row[index]
	}
	return values
}

func validTaintSHA256(value any) bool {
	text, ok := value.(string)
	if !ok || len(text) != 64 || text != strings.ToLower(text) {
		return false
	}
	decoded, err := hex.DecodeString(text)
	return err == nil && len(decoded) == 32
}

func taintStringValue(value any) string {
	text, _ := value.(string)
	return text
}

func expectedTaintProfileFingerprint(endpoint string) string {
	descriptor := struct {
		ProtocolVersion     string `json:"protocol_version"`
		Endpoint            string `json:"endpoint"`
		Model               string `json:"model"`
		Dimensions          int    `json:"dimensions"`
		WireEncoding        string `json:"wire_encoding"`
		VectorNormalization string `json:"vector_normalization"`
	}{
		ProtocolVersion:     "openai-compatible-embeddings-v1",
		Endpoint:            endpoint,
		Model:               "task13-model",
		Dimensions:          2,
		WireEncoding:        "float32-v1",
		VectorNormalization: "cosine-unit-f32-v1",
	}
	payload, err := json.Marshal(descriptor)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func expectedTaintContentDigest(chunks []source.Chunk) string {
	digest := sha256.New()
	writeTaintCanonicalString(digest, "sqlite-canonical-content-v1")
	for ordinal, chunk := range chunks {
		writeTaintCanonicalInteger(digest, int64(ordinal))
		for _, value := range []string{
			chunk.ID, chunk.Text, string(chunk.Language), chunk.SymbolName, chunk.Reference.Path,
		} {
			writeTaintCanonicalString(digest, value)
		}
		writeTaintCanonicalInteger(digest, int64(chunk.Reference.StartLine))
		writeTaintCanonicalInteger(digest, int64(chunk.Reference.EndLine))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func expectedTaintGenerationID(policyVersion, revision string, semantic bool, fingerprint string) string {
	digest := sha256.New()
	space := "lexical-only"
	if semantic {
		space = "semantic"
	}
	for _, value := range []string{"generation-v1", policyVersion, revision, space} {
		writeTaintCanonicalString(digest, value)
	}
	if semantic {
		writeTaintCanonicalString(digest, fingerprint)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func expectedTaintManifestDigest(
	repositoryID, generationID, corpusRevision, contentDigest, scanPolicyVersion,
	profileFingerprint, profileModel string,
	dimensions int,
	vectorDigest string,
) string {
	semanticState := "lexical-only"
	if profileFingerprint != "" || profileModel != "" || dimensions != 0 {
		semanticState = "semantic"
	}
	digest := sha256.New()
	for _, value := range []string{
		"generation-manifest-v1", repositoryID, generationID, corpusRevision, contentDigest,
		scanPolicyVersion, semanticState, profileFingerprint, profileModel, "cosine", vectorDigest,
	} {
		writeTaintCanonicalString(digest, value)
	}
	writeTaintCanonicalInteger(digest, int64(dimensions))
	return hex.EncodeToString(digest.Sum(nil))
}

func expectedTaintVectorDigest(chunks []source.Chunk, semantic bool) string {
	type record struct {
		chunkID string
		blob    []byte
	}
	records := make([]record, 0, len(chunks))
	if semantic {
		for _, chunk := range chunks {
			blob, present := expectedTaintVectorBlob(chunks, chunk.ID)
			if !present {
				panic("unexpected task13 chunk")
			}
			records = append(records, record{chunkID: chunk.ID, blob: blob})
		}
		sort.Slice(records, func(left, right int) bool { return records[left].chunkID < records[right].chunkID })
	}
	digest := sha256.New()
	writeTaintCanonicalString(digest, "sqlite-vector-digest-v1")
	for _, record := range records {
		writeTaintCanonicalString(digest, record.chunkID)
		writeTaintCanonicalInteger(digest, 1)
		writeTaintCanonicalInteger(digest, 2)
		writeTaintCanonicalInteger(digest, int64(len(record.blob)))
		_, _ = digest.Write(record.blob)
	}
	writeTaintCanonicalInteger(digest, int64(len(records)))
	return hex.EncodeToString(digest.Sum(nil))
}

func expectedTaintVectorBlob(chunks []source.Chunk, chunkID string) ([]byte, bool) {
	if len(chunks) != 3 {
		return nil, false
	}
	switch chunkID {
	case chunks[0].ID:
		return []byte{0x00, 0x00, 0x80, 0x3f, 0x00, 0x00, 0x00, 0x00}, true
	case chunks[1].ID:
		return []byte{0x00, 0x00, 0x80, 0x3f, 0x00, 0x00, 0x00, 0x00}, true
	case chunks[2].ID:
		return []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x80, 0x3f}, true
	default:
		return nil, false
	}
}

func writeTaintCanonicalString(writer hash.Hash, value string) {
	writeTaintCanonicalInteger(writer, int64(len(value)))
	_, _ = writer.Write([]byte(value))
}

func writeTaintCanonicalInteger(writer hash.Hash, value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = writer.Write(encoded[:])
}

func assertPermittedTaintChunks(t *testing.T, chunks []source.Chunk, forbidden []string) {
	t.Helper()
	if len(chunks) != 3 {
		t.Fatalf("stored chunks = %d, want three permitted chunks", len(chunks))
	}
	assertNoCLITaintValue(t, "stored structured chunks", chunks, forbidden)
	wantReferences := []source.Reference{
		{Path: "safe/allowed.jsx", StartLine: 1, EndLine: 1},
		{Path: "safe/allowed.py", StartLine: 1, EndLine: 2},
		{Path: "safe/allowed.ts", StartLine: 1, EndLine: 3},
	}
	wantText := permittedTaintDocumentInputs()
	wantLanguages := []source.Language{source.LanguageJavaScript, source.LanguagePython, source.LanguageTypeScript}
	wantSymbols := []string{"safeJavaScriptEntry", "permitted_python_lookup", "permittedTypeScriptLookup"}
	for index, chunk := range chunks {
		if chunk.ID == "" || chunk.Text != wantText[index] || chunk.Language != wantLanguages[index] ||
			chunk.SymbolName != wantSymbols[index] || chunk.Reference != wantReferences[index] || !chunk.Reference.Valid() {
			t.Fatalf("chunk %d = %#v, want exact permitted canonical chunk", index, chunk)
		}
	}
}

func permittedTaintDocumentInputs() []string {
	return []string{
		"export const safeJavaScriptEntry = () => \"SAFE_JAVASCRIPT_SEARCH_TOKEN\"",
		"def permitted_python_lookup():\n    return \"SAFE_PYTHON_SEARCH_TOKEN\"",
		"export function permittedTypeScriptLookup() {\n  return \"SAFE_TYPESCRIPT_SEARCH_TOKEN\"\n}",
	}
}

func assertPermittedTaintHits(
	t *testing.T,
	hits []searchdomain.Hit,
	canonical []source.Chunk,
	wantScores []float64,
	forbidden []string,
) {
	t.Helper()
	canonical = permittedNonJavaScriptChunks(canonical)
	if len(hits) != len(canonical) || len(wantScores) != len(canonical) {
		t.Fatalf("structured search hits/scores = %d/%d, want %d exact permitted results", len(hits), len(wantScores), len(canonical))
	}
	assertNoCLITaintValue(t, "structured search hits", hits, forbidden)
	want := make([]searchdomain.Hit, len(canonical))
	for index := range canonical {
		want[index] = searchdomain.Hit{Chunk: canonical[index], Score: wantScores[index]}
	}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("structured search hits = %#v, want every field exact %#v", hits, want)
	}
}

func permittedNonJavaScriptChunks(chunks []source.Chunk) []source.Chunk {
	filtered := make([]source.Chunk, 0, len(chunks))
	for _, chunk := range chunks {
		if chunk.Language != source.LanguageJavaScript {
			filtered = append(filtered, chunk)
		}
	}
	return filtered
}

func assertPermittedJavaScriptSearch(
	t *testing.T,
	code int,
	stdout, stderr string,
	hits []searchdomain.Hit,
	canonical []source.Chunk,
	forbidden []string,
) {
	t.Helper()
	if len(canonical) != 3 {
		t.Fatalf("canonical chunks = %d, want JavaScript plus prior languages", len(canonical))
	}
	wantOutput := "0.600 safe/allowed.jsx:1-1 safeJavaScriptEntry\n" + canonical[0].Text + "\n"
	if code != 0 || stdout != wantOutput || stderr != "" {
		t.Fatalf("JavaScript search = code %d stdout %q stderr %q, want exact safe hit", code, stdout, stderr)
	}
	want := []searchdomain.Hit{{Chunk: canonical[0], Score: 0.6}}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("JavaScript structured hits = %#v, want exact %#v", hits, want)
	}
	assertNoCLITaintValue(t, "JavaScript structured search hits", hits, forbidden)
}

func permittedTaintLexicalScores() []float64 {
	return []float64{0.8999999999999999, 0.6}
}

func assertExactTaintHTTPRequests(
	t *testing.T,
	requests []taintHTTPRequest,
	fixture cliTaintFixture,
	wantInputs [][]string,
	queryRequest int,
) {
	t.Helper()
	if len(requests) != len(wantInputs) || queryRequest >= len(requests) {
		t.Fatalf("HTTP requests/expectations/query index = %d/%d/%d", len(requests), len(wantInputs), queryRequest)
	}
	forbidden := appendCopy(
		fixture.forbidden,
		fixture.repository, canonicalPath(t, fixture.repository), "safe/allowed.jsx", "safe/allowed.py", "safe/allowed.ts",
	)
	metadataForbidden := appendCopy(forbidden, cliTaintQueryCanary)
	for index, request := range requests {
		if request.Method != http.MethodPost || request.URLString != "/v1/embeddings" ||
			request.Path != "/v1/embeddings" || request.RequestURI != "/v1/embeddings" || request.RawQuery != "" ||
			request.ReadError != "" || request.CloseError != "" || request.BodyOverflow || request.BodyTruncated ||
			len(request.Body) > maxTaintHTTPRequestBytes {
			t.Fatalf("request %d = %#v, want successful POST capture", index, request)
		}
		if request.ContentLength != int64(len(request.Body)) || len(request.Trailer) != 0 ||
			len(request.TransferEncoding) != 0 || request.Header.Get("Content-Type") != "application/json" ||
			request.Header.Get("Authorization") != "Bearer "+cliTaintProviderKey {
			t.Fatalf("request %d metadata = %#v, want complete exact JSON request capture", index, request)
		}
		host, _, err := net.SplitHostPort(request.Host)
		if err != nil {
			t.Fatalf("request %d host = %q: %v", index, request.Host, err)
		}
		address := net.ParseIP(host)
		if address == nil || !address.IsLoopback() {
			t.Fatalf("request %d host = %q, want numeric loopback", index, request.Host)
		}
		metadata := struct {
			Method, URLString, Path, RequestURI, RawQuery, Host string
			Header, Trailer                                     http.Header
			TransferEncoding                                    []string
			ContentLength                                       int64
			BodyOverflow, BodyTruncated                         bool
			ReadError, CloseError                               string
		}{
			Method: request.Method, URLString: request.URLString, Path: request.Path,
			RequestURI: request.RequestURI, RawQuery: request.RawQuery, Host: request.Host,
			Header: request.Header, Trailer: request.Trailer,
			TransferEncoding: request.TransferEncoding, ContentLength: request.ContentLength,
			BodyOverflow: request.BodyOverflow, BodyTruncated: request.BodyTruncated,
			ReadError: request.ReadError, CloseError: request.CloseError,
		}
		assertNoCLITaintValue(t, fmt.Sprintf("HTTP request %d metadata", index), metadata, metadataForbidden)

		decoded, err := decodeStrictTaintEmbeddingRequest(request.Body)
		if err != nil {
			t.Fatalf("decode HTTP request %d error = %v", index, err)
		}
		if !reflect.DeepEqual(decoded.Input, wantInputs[index]) {
			t.Fatalf("HTTP request %d inputs = %#v, want exact ordered %#v", index, decoded.Input, wantInputs[index])
		}
		envelope := struct {
			Model, EncodingFormat string
			Dimensions            int
		}{decoded.Model, decoded.EncodingFormat, decoded.Dimensions}
		assertNoCLITaintValue(t, fmt.Sprintf("HTTP request %d non-input JSON fields", index), envelope, metadataForbidden)

		if index != queryRequest {
			checkCLITaintBytes(t, fmt.Sprintf("HTTP request %d body", index), request.Body, metadataForbidden)
			continue
		}
		if !reflect.DeepEqual(decoded.Input, []string{cliTaintQueryCanary}) {
			t.Fatalf("HTTP request %d is designated query egress but inputs = %#v", index, decoded.Input)
		}
		allowedJSON, err := json.Marshal(cliTaintQueryCanary)
		if err != nil || bytes.Count(request.Body, allowedJSON) != 1 {
			t.Fatalf("HTTP query request contains %d exact query elements; marshal error %v", bytes.Count(request.Body, allowedJSON), err)
		}
		location := bytes.Index(request.Body, allowedJSON)
		redacted := make([]byte, 0, len(request.Body)-len(allowedJSON))
		redacted = append(redacted, request.Body[:location]...)
		redacted = append(redacted, request.Body[location+len(allowedJSON):]...)
		checkCLITaintBytes(t, "HTTP query request outside exact input element", redacted, metadataForbidden)
	}
}

type taintEmbeddingRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
	Dimensions     int      `json:"dimensions"`
}

func decodeStrictTaintEmbeddingRequest(body []byte) (taintEmbeddingRequest, error) {
	if !hasExactTaintJSONFields(body, []string{"model", "input", "encoding_format", "dimensions"}) {
		return taintEmbeddingRequest{}, errors.New("invalid synthetic request envelope")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request taintEmbeddingRequest
	if err := decoder.Decode(&request); err != nil {
		return taintEmbeddingRequest{}, errors.New("invalid synthetic request envelope")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return taintEmbeddingRequest{}, errors.New("invalid synthetic request envelope")
	}
	if request.Model != "task13-model" || request.EncodingFormat != "float" ||
		request.Dimensions != 2 || len(request.Input) == 0 {
		return taintEmbeddingRequest{}, errors.New("invalid synthetic request envelope")
	}
	return request, nil
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
	result := taintcheck.ScanValue(value, forbidden)
	if !result.Complete {
		t.Fatalf("%s structured taint inspection exceeded a bound", label)
	}
	if result.Found {
		t.Fatalf("%s contains forbidden taint %q encoded as %s", label, result.Match.Canary, result.Match.Encoding)
	}
	checkCLITaintBytes(t, label+" debug", []byte(fmt.Sprintf("%#v", value)), forbidden)
	if payload, err := json.Marshal(value); err == nil {
		checkCLITaintBytes(t, label+" JSON", payload, forbidden)
	}
}

func checkCLITaintBytes(t *testing.T, label string, payload []byte, forbidden []string) {
	t.Helper()
	result := taintcheck.Scan(payload, forbidden)
	if !result.Complete {
		t.Fatalf("%s taint inspection exceeded a bound", label)
	}
	if result.Found {
		t.Fatalf("%s contains forbidden taint %q encoded as %s", label, result.Match.Canary, result.Match.Encoding)
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
