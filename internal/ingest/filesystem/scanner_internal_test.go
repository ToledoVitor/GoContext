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
