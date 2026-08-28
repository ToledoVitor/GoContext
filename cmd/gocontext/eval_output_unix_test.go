//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest/filesystem"
)

func TestEvalOutputFailureBeforePublicationLeavesNoTargetOrPartialFile(t *testing.T) {
	directory := secureEvalOutputDirectory(t)
	target := filepath.Join(directory, "report.json")
	output, err := prepareEvalOutput(target)
	if err != nil {
		t.Fatal(err)
	}
	err = output.writeWithOperations([]byte("complete-report\n"), 1024, evalOutputOperations{
		writeFile: func(file *os.File, payload []byte) error {
			if _, err := file.Write(payload[:4]); err != nil {
				return err
			}
			return errors.New("injected partial write")
		},
	})
	if !errors.Is(err, errEvalOutput) {
		t.Fatalf("write error = %v", err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("target exists/error = %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("directory entries/error = %v/%v", entries, err)
	}
}

func TestEvalOutputPublicationNeverOverwritesCollision(t *testing.T) {
	directory := secureEvalOutputDirectory(t)
	target := filepath.Join(directory, "report.json")
	output, err := prepareEvalOutput(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := output.Write([]byte("replacement\n"), 1024); !errors.Is(err, errEvalOutput) {
		t.Fatalf("write error = %v", err)
	}
	_ = output.Close()
	payload, err := os.ReadFile(target)
	if err != nil || string(payload) != "preserve" {
		t.Fatalf("target/error = %q/%v", payload, err)
	}
}

func TestEvalOutputPreservesVisibleReportOnDurabilityAmbiguity(t *testing.T) {
	directory := secureEvalOutputDirectory(t)
	target := filepath.Join(directory, "report.json")
	output, err := prepareEvalOutput(target)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("complete-report\n")
	err = output.writeWithOperations(payload, 1024, evalOutputOperations{
		syncDir: func(*os.File) error { return errors.New("injected directory sync") },
	})
	if !errors.Is(err, errEvalOutputIndeterminate) || !output.visible {
		t.Fatalf("write error/visible = %v/%v", err, output.visible)
	}
	_ = output.Close()
	visible, err := os.ReadFile(target)
	if err != nil || string(visible) != string(payload) {
		t.Fatalf("visible report/error = %q/%v", visible, err)
	}
	if _, err := prepareEvalOutput(target); !errors.Is(err, errEvalOutput) {
		t.Fatalf("second prepare error = %v", err)
	}
}

func TestEvalOutputRejectsParentPathRetargetBeforePublication(t *testing.T) {
	directory := secureEvalOutputDirectory(t)
	target := filepath.Join(directory, "report.json")
	output, err := prepareEvalOutput(target)
	if err != nil {
		t.Fatal(err)
	}
	retained := directory + "-retained"
	err = output.writeWithOperations([]byte("complete-report\n"), 1024, evalOutputOperations{
		beforePublish: func() error {
			if err := os.Rename(directory, retained); err != nil {
				return err
			}
			return os.Mkdir(directory, 0o700)
		},
	})
	if !errors.Is(err, errEvalOutput) {
		t.Fatalf("write error = %v", err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{target, filepath.Join(retained, "report.json")} {
		if _, err := os.Lstat(candidate); !os.IsNotExist(err) {
			t.Fatalf("retargeted output exists/error = %v", err)
		}
	}
}

func TestPrivateEvalFileRejectsParentPathRetargetAfterDirectoryOpen(t *testing.T) {
	directory := secureEvalOutputDirectory(t)
	target := filepath.Join(directory, "gate.json")
	if err := os.WriteFile(target, []byte(`{"owner_authorized":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	retained := directory + "-retained"
	_, err := readPrivateEvalFileWithOperations(target, 1024, evalFileOperations{
		afterOpenParent: func() error {
			if err := os.Rename(directory, retained); err != nil {
				return err
			}
			return os.Symlink(retained, directory)
		},
	})
	if !errors.Is(err, errEvalChecklist) {
		t.Fatalf("read error = %v", err)
	}
}

func TestEvalOutputRejectsWritableAncestorBeforeCreatingTarget(t *testing.T) {
	base := secureEvalOutputDirectory(t)
	unsafeAncestor := filepath.Join(base, "unsafe")
	directory := filepath.Join(unsafeAncestor, "private")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeAncestor, 0o777); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "report.json")
	if _, err := prepareEvalOutput(target); !errors.Is(err, errEvalOutput) {
		t.Fatalf("prepare error = %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("target exists/error = %v", err)
	}
}

func TestPrivateEvalFileRejectsWritableAncestorBeforeRead(t *testing.T) {
	base := secureEvalOutputDirectory(t)
	unsafeAncestor := filepath.Join(base, "unsafe")
	directory := filepath.Join(unsafeAncestor, "private")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeAncestor, 0o777); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "gate.json")
	if err := os.WriteFile(target, []byte(`{"owner_authorized":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateEvalFileWithOperations(target, 1024, evalFileOperations{}); !errors.Is(err, errEvalChecklist) {
		t.Fatalf("read error = %v", err)
	}
}

func TestEvalOutputAllowsRootOwnedStickySystemAncestor(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "gocontext-eval-")
	if err != nil {
		t.Skipf("system temporary directory unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(canonical, 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := prepareEvalOutput(filepath.Join(canonical, "report.json"))
	if err != nil {
		t.Fatalf("prepare error = %v", err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEvalOutputPreservesCommittedReportWhenTrustChangesAfterVisibility(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(base, root, directory string) (string, error)
	}{
		{name: "path disappears", mutate: func(_, _, directory string) (string, error) {
			retained := directory + "-retained"
			return filepath.Join(retained, "report.json"), os.Rename(directory, retained)
		}},
		{name: "path replaced", mutate: func(_, _, directory string) (string, error) {
			retained := directory + "-retained"
			if err := os.Rename(directory, retained); err != nil {
				return "", err
			}
			return filepath.Join(retained, "report.json"), os.Mkdir(directory, 0o700)
		}},
		{name: "moved inside root", mutate: func(_, root, directory string) (string, error) {
			moved := filepath.Join(root, "moved-output")
			return filepath.Join(moved, "report.json"), os.Rename(directory, moved)
		}},
		{name: "privacy changes", mutate: func(_, _, directory string) (string, error) {
			return filepath.Join(directory, "report.json"), os.Chmod(directory, 0o755)
		}},
		{name: "ancestor becomes writable", mutate: func(base, _, directory string) (string, error) {
			return filepath.Join(directory, "report.json"), os.Chmod(base, 0o777)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := secureEvalOutputDirectory(t)
			root := filepath.Join(base, "root")
			directory := filepath.Join(base, "output")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			openedRoot, err := filesystem.OpenRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = openedRoot.Close() })
			output, err := prepareEvalOutput(filepath.Join(directory, "report.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := output.requireOutsideRoot(openedRoot); err != nil {
				t.Fatal(err)
			}
			payload := []byte("committed-report\n")
			var visiblePath string
			err = output.writeWithOperations(payload, 1024, evalOutputOperations{
				afterPublish: func() error {
					var mutateErr error
					visiblePath, mutateErr = test.mutate(base, root, directory)
					return mutateErr
				},
			})
			if !errors.Is(err, errEvalOutputIndeterminate) || !output.visible {
				t.Fatalf("write error/visible = %v/%v", err, output.visible)
			}
			if err := output.Close(); err != nil {
				t.Fatal(err)
			}
			visible, err := os.ReadFile(visiblePath)
			if err != nil || string(visible) != string(payload) {
				t.Fatalf("visible report/error = %q/%v", visible, err)
			}
		})
	}
}

func secureEvalOutputDirectory(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}
