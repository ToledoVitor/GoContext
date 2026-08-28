package filesystem

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest"
)

func TestScannerAppliesHardDenyBeforeOpen(t *testing.T) {
	root := t.TempDir()
	writeInternalFile(t, root, "keep.py", "print('keep')\n")
	writeInternalFile(t, root, ".env.py", "TOKEN = 'never-open'\n")
	writeInternalFile(t, root, ".github/workflow.ts", "export const token = 'never-open'\n")
	writeInternalFile(t, root, "credentials.py", "TOKEN = 'never-open'\n")

	scanner := &Scanner{
		openPath: func(directory repositoryHandle, name string, wantDirectory bool) (repositoryHandle, error) {
			if reason, denied := classifyPath(path.Base(name), wantDirectory); denied {
				return nil, fmt.Errorf("opened hard-denied path with reason %s", reason)
			}
			return openNoFollow(directory, name, wantDirectory)
		},
	}
	result, err := scanner.Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(result.Files) != 1 || result.Files[0].Reference.Path != "keep.py" {
		t.Fatalf("Scan() files = %#v, want [keep.py]", result.Files)
	}
}

func TestScannerAggregatesUnsupportedMetadataWithoutOpeningContent(t *testing.T) {
	root := t.TempDir()
	writeInternalFile(t, root, "notes.md", "metadata-only")
	fileOpens := 0
	scanner := &Scanner{
		openPath: func(directory repositoryHandle, name string, wantDirectory bool) (repositoryHandle, error) {
			if !wantDirectory {
				fileOpens++
			}
			return openNoFollow(directory, name, wantDirectory)
		},
	}
	result, err := scanner.Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if fileOpens != 0 {
		t.Fatalf("unsupported file opens = %d, want zero", fileOpens)
	}
	if result.Report.UnsupportedByExtension[".md"] != 1 || result.Report.UnsupportedBytesByExtension[".md"] != 13 {
		t.Fatalf("unsupported aggregate = %#v/%#v", result.Report.UnsupportedByExtension, result.Report.UnsupportedBytesByExtension)
	}
}

func TestScannerFailsClosedWhenAllowedFileCannotBeRead(t *testing.T) {
	root := t.TempDir()
	writeInternalFile(t, root, "allowed.py", "read-error-canary\n")
	scanner := &Scanner{
		openPath: func(directory repositoryHandle, name string, wantDirectory bool) (repositoryHandle, error) {
			if wantDirectory {
				return openNoFollow(directory, name, true)
			}
			file, err := openNoFollow(directory, name, false)
			if err != nil {
				return nil, err
			}
			return &readErrorFile{File: file}, nil
		},
	}

	_, err := scanner.Scan(context.Background(), root)
	if err == nil {
		t.Fatal("Scan() error = nil, want fail-closed read error")
	}
	if !strings.Contains(err.Error(), "read-file") {
		t.Fatalf("Scan() error = %q, want read-file category", err)
	}
	for _, sensitive := range []string{"allowed.py", "read-error-canary", "low-level-read-canary"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Errorf("Scan() error = %q, must not expose %q", err, sensitive)
		}
	}
}

func TestScannerRejectsFileSwappedToSymlinkBeforeOpen(t *testing.T) {
	root := t.TempDir()
	writeInternalFile(t, root, ".env.py", "SECRET_CANARY = 'must-not-be-read'\n")
	writeInternalFile(t, root, "allowed.py", "print('safe')\n")

	scanner := &Scanner{
		openPath: func(directory repositoryHandle, name string, wantDirectory bool) (repositoryHandle, error) {
			if !wantDirectory && name == "allowed.py" {
				if err := os.Remove(filepath.Join(root, name)); err != nil {
					return nil, err
				}
				if err := os.Symlink(".env.py", filepath.Join(root, name)); err != nil {
					return nil, err
				}
			}
			return openNoFollow(directory, name, wantDirectory)
		},
	}
	_, err := scanner.Scan(context.Background(), root)
	if err == nil {
		t.Fatal("Scan() error = nil, want fail-closed swap error")
	}
	if !strings.Contains(err.Error(), "open-file") {
		t.Fatalf("Scan() error = %q, want no-follow open category", err)
	}
	for _, sensitive := range []string{"allowed.py", ".env.py", "SECRET_CANARY", "must-not-be-read"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Errorf("Scan() error = %q, must not expose %q", err, sensitive)
		}
	}
}

func TestScannerRejectsRepositoryRootSymlink(t *testing.T) {
	realRoot := t.TempDir()
	writeInternalFile(t, realRoot, "safe.py", "print('safe')\n")
	alias := filepath.Join(t.TempDir(), "root-alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	_, err := NewScanner().Scan(context.Background(), alias)
	if err == nil || !strings.Contains(err.Error(), "open root failed") {
		t.Fatalf("Scan() error = %v, want sanitized no-follow root failure", err)
	}
}

func TestScanOpenedUsesRetainedRootWhenPathIsReplaced(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	replacement := filepath.Join(base, "replacement")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	writeInternalFile(t, root, "approved.py", "print('approved')\n")
	writeInternalFile(t, replacement, "substituted.py", "print('substituted')\n")

	opened, err := OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	if err := os.Rename(root, filepath.Join(base, "approved-retained")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, root); err != nil {
		t.Fatal(err)
	}

	result, err := NewScanner().ScanOpened(context.Background(), opened)
	if err != nil {
		t.Fatalf("ScanOpened() error = %v", err)
	}
	if len(result.Files) != 1 || result.Files[0].Reference.Path != "approved.py" {
		t.Fatalf("ScanOpened() files = %#v, want retained approved.py", result.Files)
	}
	if opened.MatchesPath(root) {
		t.Fatal("MatchesPath(replaced root) = true")
	}
}

func TestScanOpenedRejectsReuse(t *testing.T) {
	root := t.TempDir()
	opened, err := OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close() })
	if _, err := NewScanner().ScanOpened(context.Background(), opened); err != nil {
		t.Fatal(err)
	}
	if _, err := NewScanner().ScanOpened(context.Background(), opened); err == nil {
		t.Fatal("second ScanOpened() error = nil")
	}
}

type readErrorFile struct {
	*os.File
}

func (f *readErrorFile) Read([]byte) (int, error) {
	return 0, errors.New("low-level-read-canary")
}

func TestValidRelativePathRejectsTraversalAndRootChanges(t *testing.T) {
	tests := []string{
		"", ".", "..", "../secret.py", "src/../../secret.py", "/absolute.py", `C:\\absolute.py`,
		`src\\secret.py`, "src//app.py", "src/./app.py", "src/\u200bapp.py", "src/\u0001app.py", string([]byte{'x', 0xff, '.', 'p', 'y'}),
	}
	for _, relativePath := range tests {
		if validRelativePath(relativePath) {
			t.Errorf("validRelativePath(%q) = true, want false", relativePath)
		}
	}
	if !validRelativePath("src/ação.py") {
		t.Fatal("validRelativePath(valid Unicode path) = false, want true")
	}
}

func TestScannerInspectsUnknownTypesBeforeAllowlist(t *testing.T) {
	root := t.TempDir()
	writeInternalFile(t, root, "container/nested/keep.py", "print('safe')\n")
	writeInternalFile(t, root, "container/.github/secret.ts", "export const hidden = true\n")
	scanner := &Scanner{
		openPath: func(directory repositoryHandle, name string, wantDirectory bool) (repositoryHandle, error) {
			file, err := openNoFollow(directory, name, wantDirectory)
			if err != nil {
				return nil, err
			}
			if wantDirectory && name == "container" {
				return &unknownTypeDirectory{File: file}, nil
			}
			return file, nil
		},
	}

	result, err := scanner.Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(result.Files) != 1 || result.Files[0].Reference.Path != "container/nested/keep.py" {
		t.Fatalf("Scan() files = %#v, want nested keep.py", result.Files)
	}
	if result.Report.Excluded[ingest.ExclusionSecurity] != 1 {
		t.Errorf("security exclusions = %d, want 1", result.Report.Excluded[ingest.ExclusionSecurity])
	}
	if result.Report.UnsupportedByExtension["<other>"] != 0 {
		t.Errorf("unsupported <other> = %d, want 0 for denied .github directory", result.Report.UnsupportedByExtension["<other>"])
	}
}

type unknownTypeDirectory struct {
	*os.File
}

func (d *unknownTypeDirectory) ReadDir(n int) ([]os.DirEntry, error) {
	entries, err := d.File.ReadDir(n)
	unknown := make([]os.DirEntry, len(entries))
	for index, entry := range entries {
		unknown[index] = unknownTypeEntry{DirEntry: entry}
	}
	return unknown, err
}

type unknownTypeEntry struct {
	os.DirEntry
}

func (e unknownTypeEntry) Type() os.FileMode { return 0 }
func (e unknownTypeEntry) IsDir() bool       { return false }

func writeInternalFile(t *testing.T, root, relativePath, content string) {
	t.Helper()
	absolutePath := filepath.Join(root, relativePath)
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(absolutePath, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
