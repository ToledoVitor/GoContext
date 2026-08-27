package filesystem_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest/filesystem"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func TestScannerIncludesSupportedFilesWithRepositoryRelativeReferences(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app.py", "def main():\n    return 1\n")
	writeFile(t, root, "src/app.ts", "export function main() {\n  return 1\n}\n")
	writeFile(t, root, "src/view.tsx", "export const View = () => <div />\n")

	files, err := filesystem.NewScanner().Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}

	want := []struct {
		path     string
		language source.Language
		endLine  int
	}{
		{path: "app.py", language: source.LanguagePython, endLine: 2},
		{path: "src/app.ts", language: source.LanguageTypeScript, endLine: 3},
		{path: "src/view.tsx", language: source.LanguageTypeScript, endLine: 1},
	}
	if len(files) != len(want) {
		t.Fatalf("Scan() returned %d files, want %d", len(files), len(want))
	}
	for i, expected := range want {
		if got := files[i].Reference.Path; got != expected.path {
			t.Errorf("files[%d].Reference.Path = %q, want %q", i, got, expected.path)
		}
		if got := files[i].Language; got != expected.language {
			t.Errorf("files[%d].Language = %q, want %q", i, got, expected.language)
		}
		if got := files[i].Reference.StartLine; got != 1 {
			t.Errorf("files[%d].Reference.StartLine = %d, want 1", i, got)
		}
		if got := files[i].Reference.EndLine; got != expected.endLine {
			t.Errorf("files[%d].Reference.EndLine = %d, want %d", i, got, expected.endLine)
		}
		if !files[i].Reference.Valid() {
			t.Errorf("files[%d].Reference = %#v, want valid reference", i, files[i].Reference)
		}
	}
}

func TestScannerExcludesUnsupportedFilesAndIgnoredDirectories(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "keep.py", "print('keep')\n")
	writeFile(t, root, "README.md", "ignore\n")

	for _, directory := range []string{
		".git", ".gocontext", ".next", ".venv", "__pycache__",
		"build", "coverage", "dist", "node_modules", "vendor", "venv",
	} {
		writeFile(t, root, filepath.Join(directory, "ignored.ts"), "export const ignored = true\n")
	}

	files, err := filesystem.NewScanner().Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(files) != 1 || files[0].Reference.Path != "keep.py" {
		t.Fatalf("Scan() paths = %v, want [keep.py]", filePaths(files))
	}
}

func TestScannerExcludesBinaryAndOversizedFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "keep.py", "print('keep')\n")
	writeFileBytes(t, root, "binary.py", []byte{'p', 'r', 'i', 'n', 't', 0, 'x'})
	writeFileBytes(t, root, "large.ts", bytes.Repeat([]byte{'x'}, int(filesystem.DefaultMaxFileSize)+1))

	files, err := filesystem.NewScanner().Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(files) != 1 || files[0].Reference.Path != "keep.py" {
		t.Fatalf("Scan() paths = %v, want [keep.py]", filePaths(files))
	}
}

func TestScannerDoesNotFollowSymlinksOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "secret.py", "SECRET = true\n")
	if err := os.Symlink(filepath.Join(outside, "secret.py"), filepath.Join(root, "linked.py")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked-dir")); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}

	files, err := filesystem.NewScanner().Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("Scan() paths = %v, want no files", filePaths(files))
	}
}

func TestScannerRejectsInvalidRootAndCancellation(t *testing.T) {
	scanner := filesystem.NewScanner()
	if _, err := scanner.Scan(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("Scan() missing root error = nil, want error")
	}

	fileRoot := filepath.Join(t.TempDir(), "file.py")
	if err := os.WriteFile(fileRoot, []byte("print('x')\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := scanner.Scan(context.Background(), fileRoot); err == nil {
		t.Fatal("Scan() file root error = nil, want error")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := scanner.Scan(ctx, t.TempDir()); err == nil {
		t.Fatal("Scan() canceled context error = nil, want error")
	}
}

func writeFile(t *testing.T, root, relativePath, content string) {
	t.Helper()
	writeFileBytes(t, root, relativePath, []byte(content))
}

func writeFileBytes(t *testing.T, root, relativePath string, content []byte) {
	t.Helper()
	path := filepath.Join(root, relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", relativePath, err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", relativePath, err)
	}
}

func filePaths(files []source.File) []string {
	paths := make([]string, len(files))
	for i := range files {
		paths[i] = files[i].Reference.Path
	}
	return paths
}
