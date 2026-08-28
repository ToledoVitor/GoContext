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
	taintSecurityCanary      = "TAINT_SECURITY_CANARY_TASK13"
	taintDependencyCanary    = "TAINT_DEPENDENCY_CANARY_TASK13"
	taintNewDependencyCanary = "TAINT_NEW_DEPENDENCY_CANARY_TASK14C"
	taintNestedCanary        = "TAINT_NESTED_CANARY_TASK13"
	taintSymlinkCanary       = "TAINT_SYMLINK_CANARY_TASK13"
	taintBinaryCanary        = "TAINT_BINARY_CANARY_TASK13"
	taintTooLargeCanary      = "TAINT_TOO_LARGE_CANARY_TASK13"
	taintGeneratedCanary     = "TAINT_GENERATED_CANARY_TASK13"
	taintUnsupportedCanary   = "TAINT_UNSUPPORTED_CANARY_TASK13"
	taintInvalidUTF8Canary   = "TAINT_INVALID_UTF8_CANARY_TASK13"
	taintPEMCanary           = "TAINT_PEM_CONTENT_CANARY_TASK13"
	taintPythonSecretCanary  = "TAINT_PYTHON_SECRET_CANARY_TASK13"
	taintTypedSecretCanary   = "TAINT_TYPED_SECRET_CANARY_TASK13"
	taintJavaScriptCanary    = "TAINT_JAVASCRIPT_EXCLUDED_CANARY_TASK14D"
	taintControlPathCanary   = "TAINT_CONTROL_PATH_CANARY_TASK13"
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
	if result.Report.IncludedFiles != 3 || result.Report.IncludedByLanguage[source.LanguageJavaScript] != 1 ||
		result.Report.IncludedByLanguage[source.LanguagePython] != 1 ||
		result.Report.IncludedByLanguage[source.LanguageTypeScript] != 1 {
		t.Fatalf("included report = %#v, want one permitted JavaScript, Python and TypeScript file", result.Report)
	}
	if got, want := filePaths(result.Files), []string{"safe/allowed.jsx", "safe/allowed.py", "safe/allowed.ts"}; !reflect.DeepEqual(got, want) {
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

	if len(allSymbols) != 3 || len(allChunks) != 3 {
		t.Fatalf("pipeline produced %d symbols and %d chunks, want 3 and 3", len(allSymbols), len(allChunks))
	}
	corpus, err := source.NewCorpus(ingest.ScanPolicyVersion, allChunks)
	if err != nil {
		t.Fatalf("NewCorpus() error = %v", err)
	}
	assertNoTaintValue(t, "canonical corpus", corpus, fixture.forbidden)
	if corpus.Revision == "" || !strings.Contains(corpus.Chunks[0].Text, "SAFE_JAVASCRIPT_SEARCH_TOKEN") ||
		!strings.Contains(corpus.Chunks[1].Text, "SAFE_PYTHON_SEARCH_TOKEN") ||
		!strings.Contains(corpus.Chunks[2].Text, "SAFE_TYPESCRIPT_SEARCH_TOKEN") {
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
	writeFile(t, root, "safe/allowed.jsx", "export const safeJavaScriptEntry = () => \"SAFE_JAVASCRIPT_SEARCH_TOKEN\"\n")

	for _, path := range []string{
		".env", ".env.local", ".env.jsx", ".git/config", ".github/workflows/ci.js",
		"credentials.js", "secret.jsx", "material.pem", "certificate.crt",
	} {
		writeFile(t, root, path, taintSecurityCanary+taintJavaScriptCanary+"\n")
	}
	for _, path := range []string{
		"node_modules/dependency.js", "vendor/vendor.jsx", "build/output.js", ".cache/cache.jsx",
	} {
		writeFile(t, root, path, taintDependencyCanary+taintJavaScriptCanary+"\n")
	}
	newDependencyPaths := []string{
		"Pods/dependency.js", ".gradle/dependency.jsx", ".dart_tool/dependency.js", ".pub-cache/dependency.jsx",
		"DerivedData/dependency.js", "Carthage/dependency.jsx", ".cxx/dependency.js", ".expo/dependency.jsx",
		".turbo/dependency.js", ".nx/dependency.jsx", ".parcel-cache/dependency.js", ".vite/dependency.jsx",
		".bundle/dependency.js",
	}
	for index, path := range newDependencyPaths {
		writeFile(t, root, path, fmt.Sprintf("%s_%02d\n", taintNewDependencyCanary, index))
	}
	writeFile(t, root, "nested-repository/.git/config", taintNestedCanary+"\n")
	writeFile(t, root, "nested-repository/00-child.js", "export function "+taintNestedCanary+"() { return '"+taintJavaScriptCanary+"' }\n")

	writeFile(t, root, "client.generated.js", taintGeneratedCanary+taintJavaScriptCanary+"\n")
	writeFile(t, root, "generated-header.jsx", "// Code generated for "+taintGeneratedCanary+taintJavaScriptCanary+". DO NOT EDIT.\nconst value = 1\n")
	writeFileBytes(t, root, "binary.js", append([]byte(taintBinaryCanary+taintJavaScriptCanary), 0, 'x'))
	oversized := append([]byte(taintTooLargeCanary+taintJavaScriptCanary), bytes.Repeat([]byte{'x'}, int(filesystem.DefaultMaxFileSize)+1)...)
	writeFileBytes(t, root, "oversized.jsx", oversized)
	writeFileBytes(t, root, "invalid-utf8.js", append([]byte(taintInvalidUTF8Canary+taintJavaScriptCanary), 0xff))
	writeFile(t, root, "opaque.TAINT_UNSUPPORTED_NAME_TASK13", taintUnsupportedCanary+"\n")
	writeFile(t, root, "pem-material.py", "MATERIAL = '''-----BEGIN PRIVATE KEY-----\n"+taintPEMCanary+"\n-----END PRIVATE KEY-----'''\n")
	writeFile(t, root, "embedded-secret.py", "safe = \"x\"; password: str = \""+taintPythonSecretCanary+"\"\n")
	writeFile(t, root, "embedded-secret.jsx", "const safe = \"x\"; const token = \""+taintTypedSecretCanary+taintJavaScriptCanary+"\"\n")

	symlinkCount := 0
	securityCount := 9
	if runtime.GOOS != "windows" {
		writeFile(t, outside, "outside.jsx", taintSymlinkCanary+taintJavaScriptCanary+"\n")
		writeFile(t, root, ".env.internal-link", taintSymlinkCanary+"\n")
		if err := os.Symlink(filepath.Join(outside, "outside.jsx"), filepath.Join(root, "external-link.jsx")); err != nil {
			t.Fatalf("Symlink(external) error = %v", err)
		}
		if err := os.Symlink(".env.internal-link", filepath.Join(root, "internal-link.js")); err != nil {
			t.Fatalf("Symlink(internal) error = %v", err)
		}
		symlinkCount = 2
		securityCount++
	}

	forbidden := []string{
		taintSecurityCanary, taintDependencyCanary, taintNewDependencyCanary, taintNestedCanary, taintBinaryCanary,
		taintTooLargeCanary, taintGeneratedCanary, taintUnsupportedCanary, taintInvalidUTF8Canary,
		taintPEMCanary, taintPythonSecretCanary, taintTypedSecretCanary, taintJavaScriptCanary,
		".env", ".env.local", ".env.jsx", ".git/config", ".github/workflows/ci.js",
		"credentials.js", "secret.jsx", "material.pem", "certificate.crt",
		"node_modules/dependency.js", "vendor/vendor.jsx", "build/output.js", ".cache/cache.jsx",
		"nested-repository/.git/config", "nested-repository/00-child.js", "client.generated.js",
		"generated-header.jsx", "binary.js", "oversized.jsx", "invalid-utf8.js",
		"opaque.TAINT_UNSUPPORTED_NAME_TASK13", "pem-material.py", "embedded-secret.py", "embedded-secret.jsx",
	}
	forbidden = append(forbidden, newDependencyPaths...)
	if symlinkCount != 0 {
		forbidden = append(
			forbidden,
			taintSymlinkCanary,
			".env.internal-link",
			"external-link.jsx",
			"internal-link.js",
			filepath.Join(outside, "outside.jsx"),
		)
	}

	wantReport := map[ingest.ExclusionReason]int{
		ingest.ExclusionSecurity:             securityCount,
		ingest.ExclusionDependencyBuildCache: 4 + len(newDependencyPaths),
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
	result := taintcheck.ScanValue(value, forbidden)
	if !result.Complete {
		t.Fatalf("%s structured taint inspection exceeded a bound", label)
	}
	if result.Found {
		t.Fatalf("%s contains forbidden taint %q encoded as %s", label, result.Match.Canary, result.Match.Encoding)
	}
	debug := []byte(fmt.Sprintf("%#v", value))
	checkTaintBytes(t, label+" debug", debug, forbidden)
	if payload, err := json.Marshal(value); err == nil {
		checkTaintBytes(t, label+" JSON", payload, forbidden)
	}
}

func checkTaintBytes(t *testing.T, label string, payload []byte, forbidden []string) {
	t.Helper()
	result := taintcheck.Scan(payload, forbidden)
	if !result.Complete {
		t.Fatalf("%s taint inspection exceeded a bound", label)
	}
	if result.Found {
		t.Fatalf("%s contains forbidden taint %q encoded as %s", label, result.Match.Canary, result.Match.Encoding)
	}
}
