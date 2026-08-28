package filesystem_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/ingest/filesystem"
	"github.com/ToledoVitor/GoContext/internal/ingest/lineparser"
	"github.com/ToledoVitor/GoContext/internal/ingest/symbolchunker"
	"github.com/ToledoVitor/GoContext/internal/source"
	"github.com/ToledoVitor/GoContext/internal/testsupport/taintcheck"
)

const (
	taintSecurityCanary     = "TAINT_SECURITY_CANARY_TASK13"
	taintDependencyCanary   = "TAINT_DEPENDENCY_CANARY_TASK13"
	taintNestedCanary       = "TAINT_NESTED_CANARY_TASK13"
	taintSymlinkCanary      = "TAINT_SYMLINK_CANARY_TASK13"
	taintBinaryCanary       = "TAINT_BINARY_CANARY_TASK13"
	taintTooLargeCanary     = "TAINT_TOO_LARGE_CANARY_TASK13"
	taintGeneratedCanary    = "TAINT_GENERATED_CANARY_TASK13"
	taintUnsupportedCanary  = "TAINT_UNSUPPORTED_CANARY_TASK13"
	taintInvalidUTF8Canary  = "TAINT_INVALID_UTF8_CANARY_TASK13"
	taintPEMCanary          = "TAINT_PEM_CONTENT_CANARY_TASK13"
	taintPythonSecretCanary = "TAINT_PYTHON_SECRET_CANARY_TASK13"
	taintTypedSecretCanary  = "TAINT_TYPED_SECRET_CANARY_TASK13"
	taintControlPathCanary  = "TAINT_CONTROL_PATH_CANARY_TASK13"
)

type scannerTaintFixture struct {
	root       string
	forbidden  []string
	wantReport map[ingest.ExclusionReason]int
}

func TestExcludedContentCannotCrossScannerParserChunkerBoundary(t *testing.T) {
	fixture := newScannerTaintFixture(t)

	result, err := filesystem.NewScanner().Scan(context.Background(), fixture.root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if result.PolicyVersion != ingest.ScanPolicyVersion {
		t.Fatalf("PolicyVersion = %q, want %q", result.PolicyVersion, ingest.ScanPolicyVersion)
	}
	if !reflect.DeepEqual(result.Report.Excluded, fixture.wantReport) {
		t.Fatalf("Excluded = %#v, want %#v", result.Report.Excluded, fixture.wantReport)
	}
	if result.Report.UnsupportedByExtension["<other>"] != 1 || len(result.Report.UnsupportedByExtension) != 1 {
		t.Fatalf("UnsupportedByExtension = %#v, want only <other>:1", result.Report.UnsupportedByExtension)
	}
	if result.Report.IncludedFiles != 2 || result.Report.IncludedByLanguage[source.LanguagePython] != 1 ||
		result.Report.IncludedByLanguage[source.LanguageTypeScript] != 1 {
		t.Fatalf("included report = %#v, want one permitted Python and one permitted TypeScript file", result.Report)
	}
	if got, want := filePaths(result.Files), []string{"safe/allowed.py", "safe/allowed.ts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Scan() paths = %v, want %v", got, want)
	}
	assertAggregateOnlyReport(t, result.Report)
	assertNoTaintValue(t, "scan result", result, fixture.forbidden)
	assertNoTaintValue(t, "scan report", result.Report, fixture.forbidden)

	parser := lineparser.NewParser()
	chunker := symbolchunker.NewChunker()
	var allSymbols []source.Symbol
	var allChunks []source.Chunk
	for _, file := range result.Files {
		assertNoTaintValue(t, "parser input", file, fixture.forbidden)
		if !file.Reference.Valid() {
			t.Fatalf("parser input reference = %#v, want valid", file.Reference)
		}
		symbols, err := parser.Parse(context.Background(), file)
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", file.Reference.Path, err)
		}
		assertNoTaintValue(t, "parser symbols", symbols, fixture.forbidden)
		for _, symbol := range symbols {
			if !symbol.Reference.Valid() || symbol.Reference.Path != file.Reference.Path {
				t.Fatalf("symbol reference = %#v, want valid provenance for %q", symbol.Reference, file.Reference.Path)
			}
		}
		chunks, err := chunker.Chunk(context.Background(), file, symbols)
		if err != nil {
			t.Fatalf("Chunk(%q) error = %v", file.Reference.Path, err)
		}
		assertNoTaintValue(t, "chunker output", chunks, fixture.forbidden)
		for _, chunk := range chunks {
			if chunk.ID == "" || !chunk.Reference.Valid() || chunk.Reference.Path != file.Reference.Path {
				t.Fatalf("chunk = %#v, want ID and valid canonical provenance", chunk)
			}
		}
		allSymbols = append(allSymbols, symbols...)
		allChunks = append(allChunks, chunks...)
	}

	if len(allSymbols) != 2 || len(allChunks) != 2 {
		t.Fatalf("pipeline produced %d symbols and %d chunks, want 2 and 2", len(allSymbols), len(allChunks))
	}
	corpus, err := source.NewCorpus(ingest.ScanPolicyVersion, allChunks)
	if err != nil {
		t.Fatalf("NewCorpus() error = %v", err)
	}
	assertNoTaintValue(t, "canonical corpus", corpus, fixture.forbidden)
	if corpus.Revision == "" || !strings.Contains(corpus.Chunks[0].Text, "SAFE_PYTHON_SEARCH_TOKEN") ||
		!strings.Contains(corpus.Chunks[1].Text, "SAFE_TYPESCRIPT_SEARCH_TOKEN") {
		t.Fatalf("canonical corpus = %#v, want functional permitted content and revision", corpus)
	}
	for _, chunk := range corpus.Chunks {
		if !chunk.Reference.Valid() {
			t.Fatalf("corpus chunk reference = %#v, want valid", chunk.Reference)
		}
	}
}

func TestScannerPathFailureExposesOnlyFixedCategoryAndOpaqueReference(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "00-safe.py", "def safe_before_failure():\n    return True\n")
	unsafeName := "zz-" + taintControlPathCanary + "\u200b.py"
	unsafePath := filepath.Join(root, unsafeName)
	if err := os.WriteFile(unsafePath, []byte(taintControlPathCanary), 0o600); err != nil {
		t.Skipf("synthetic filesystem does not permit the control-path fixture: %v", err)
	}

	result, err := filesystem.NewScanner().Scan(context.Background(), root)
	if err == nil {
		t.Fatal("Scan() error = nil, want fail-closed invalid path")
	}
	if result.PolicyVersion != "" || len(result.Files) != 0 || result.Report.Excluded != nil {
		t.Fatalf("Scan() result = %#v, want no partial safe corpus after scanner error", result)
	}
	if !strings.Contains(err.Error(), "invalid-path [ref:") {
		t.Fatalf("Scan() error = %q, want fixed invalid-path category and opaque reference", err)
	}
	assertNoTaintValue(t, "scanner path error", err, []string{taintControlPathCanary, unsafeName, unsafePath})
}

func newScannerTaintFixture(t *testing.T) scannerTaintFixture {
	t.Helper()
	root := t.TempDir()
	outside := t.TempDir()

	writeFile(t, root, "safe/allowed.py", "def permitted_python_lookup():\n    return \"SAFE_PYTHON_SEARCH_TOKEN\"\n")
	writeFile(t, root, "safe/allowed.ts", "export function permittedTypeScriptLookup() {\n  return \"SAFE_TYPESCRIPT_SEARCH_TOKEN\"\n}\n")

	for _, path := range []string{
		".env", ".env.local", ".env.ts", ".git/config", ".github/workflows/ci.ts",
		"credentials.py", "secret.ts", "material.pem", "certificate.crt",
	} {
		writeFile(t, root, path, taintSecurityCanary+"\n")
	}
	for _, path := range []string{
		"node_modules/dependency.ts", "vendor/vendor.py", "build/output.ts", ".cache/cache.py",
	} {
		writeFile(t, root, path, taintDependencyCanary+"\n")
	}
	writeFile(t, root, "nested-repository/.git/config", taintNestedCanary+"\n")
	writeFile(t, root, "nested-repository/00-child.py", "def "+taintNestedCanary+"():\n    return True\n")

	writeFile(t, root, "client.generated.ts", taintGeneratedCanary+"\n")
	writeFile(t, root, "generated-header.py", "# Code generated for "+taintGeneratedCanary+". DO NOT EDIT.\nVALUE = 1\n")
	writeFileBytes(t, root, "binary.py", append([]byte(taintBinaryCanary), 0, 'x'))
	oversized := append([]byte(taintTooLargeCanary), bytes.Repeat([]byte{'x'}, int(filesystem.DefaultMaxFileSize)+1)...)
	writeFileBytes(t, root, "oversized.ts", oversized)
	writeFileBytes(t, root, "invalid-utf8.py", append([]byte(taintInvalidUTF8Canary), 0xff))
	writeFile(t, root, "opaque.TAINT_UNSUPPORTED_NAME_TASK13", taintUnsupportedCanary+"\n")
	writeFile(t, root, "pem-material.py", "MATERIAL = '''-----BEGIN PRIVATE KEY-----\n"+taintPEMCanary+"\n-----END PRIVATE KEY-----'''\n")
	writeFile(t, root, "embedded-secret.py", "safe = \"x\"; password: str = \""+taintPythonSecretCanary+"\"\n")
	writeFile(t, root, "embedded-secret.ts", "const safe = \"x\"; const token: string = \""+taintTypedSecretCanary+"\"\n")

	symlinkCount := 0
	securityCount := 9
	if runtime.GOOS != "windows" {
		writeFile(t, outside, "outside.py", taintSymlinkCanary+"\n")
		writeFile(t, root, ".env.internal-link", taintSymlinkCanary+"\n")
		if err := os.Symlink(filepath.Join(outside, "outside.py"), filepath.Join(root, "external-link.py")); err != nil {
			t.Fatalf("Symlink(external) error = %v", err)
		}
		if err := os.Symlink(".env.internal-link", filepath.Join(root, "internal-link.ts")); err != nil {
			t.Fatalf("Symlink(internal) error = %v", err)
		}
		symlinkCount = 2
		securityCount++
	}

	forbidden := []string{
		taintSecurityCanary, taintDependencyCanary, taintNestedCanary, taintBinaryCanary,
		taintTooLargeCanary, taintGeneratedCanary, taintUnsupportedCanary, taintInvalidUTF8Canary,
		taintPEMCanary, taintPythonSecretCanary, taintTypedSecretCanary,
		".env", ".env.local", ".env.ts", ".git/config", ".github/workflows/ci.ts",
		"credentials.py", "secret.ts", "material.pem", "certificate.crt",
		"node_modules/dependency.ts", "vendor/vendor.py", "build/output.ts", ".cache/cache.py",
		"nested-repository/.git/config", "nested-repository/00-child.py", "client.generated.ts",
		"generated-header.py", "binary.py", "oversized.ts", "invalid-utf8.py",
		"opaque.TAINT_UNSUPPORTED_NAME_TASK13", "pem-material.py", "embedded-secret.py", "embedded-secret.ts",
	}
	if symlinkCount != 0 {
		forbidden = append(
			forbidden,
			taintSymlinkCanary,
			".env.internal-link",
			"external-link.py",
			"internal-link.ts",
			filepath.Join(outside, "outside.py"),
		)
	}

	wantReport := map[ingest.ExclusionReason]int{
		ingest.ExclusionSecurity:             securityCount,
		ingest.ExclusionDependencyBuildCache: 4,
		ingest.ExclusionNestedRepository:     1,
		ingest.ExclusionUnsupportedExtension: 1,
		ingest.ExclusionTooLarge:             1,
		ingest.ExclusionBinary:               1,
		ingest.ExclusionInvalidUTF8:          1,
		ingest.ExclusionGenerated:            2,
		ingest.ExclusionSecret:               3,
	}
	if symlinkCount != 0 {
		wantReport[ingest.ExclusionSymlink] = symlinkCount
	}
	return scannerTaintFixture{
		root:       root,
		forbidden:  forbidden,
		wantReport: wantReport,
	}
}

func assertAggregateOnlyReport(t *testing.T, report ingest.ScanReport) {
	t.Helper()
	payload, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal(report) error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("Unmarshal(report) error = %v", err)
	}
	for _, forbiddenField := range []string{"path", "name", "detail", "content", "example"} {
		if _, present := decoded[forbiddenField]; present {
			t.Fatalf("report field %q is present in %s", forbiddenField, payload)
		}
		for key := range decoded {
			if strings.EqualFold(key, forbiddenField) {
				t.Fatalf("report field %q is present in %s", key, payload)
			}
		}
	}
}

func assertNoTaintValue(t *testing.T, label string, value any, forbidden []string) {
	t.Helper()
	assertNoTaintReflect(t, label, reflect.ValueOf(value), forbidden)
	debug := []byte(fmt.Sprintf("%#v", value))
	checkTaintBytes(t, label+" debug", debug, forbidden)
	if payload, err := json.Marshal(value); err == nil {
		checkTaintBytes(t, label+" JSON", payload, forbidden)
	}
}

func assertNoTaintReflect(t *testing.T, label string, value reflect.Value, forbidden []string) {
	t.Helper()
	if !value.IsValid() {
		return
	}
	if value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if !value.IsNil() {
			assertNoTaintReflect(t, label, value.Elem(), forbidden)
		}
		return
	}
	if value.Kind() == reflect.String {
		checkTaintBytes(t, label+" string", []byte(value.String()), forbidden)
		return
	}
	if value.Kind() == reflect.Slice && value.Type().Elem().Kind() == reflect.Uint8 {
		checkTaintBytes(t, label+" bytes", value.Bytes(), forbidden)
		return
	}
	switch value.Kind() {
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			assertNoTaintReflect(t, label, value.Field(index), forbidden)
		}
	case reflect.Slice, reflect.Array:
		for index := 0; index < value.Len(); index++ {
			assertNoTaintReflect(t, label, value.Index(index), forbidden)
		}
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			assertNoTaintReflect(t, label, iterator.Key(), forbidden)
			assertNoTaintReflect(t, label, iterator.Value(), forbidden)
		}
	}
}

func checkTaintBytes(t *testing.T, label string, payload []byte, forbidden []string) {
	t.Helper()
	if match, found := taintcheck.Find(payload, forbidden); found {
		t.Fatalf("%s contains forbidden taint %q encoded as %s", label, match.Canary, match.Encoding)
	}
}
