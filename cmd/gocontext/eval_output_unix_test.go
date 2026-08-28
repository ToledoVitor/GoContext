//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	assertNoEvalTemporaryFiles(t, directory)
}

func TestEvalOutputRejectsTargetMutationAfterPublicationWithoutRemovingRecoveryLink(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(target string) error
		want    string
		recover bool
		entries int
	}{
		{
			name: "target unlinked",
			mutate: func(target string) error {
				return os.Remove(target)
			},
			recover: true,
			entries: 1,
		},
		{
			name: "target replaced with same-size private file",
			mutate: func(target string) error {
				if err := os.Remove(target); err != nil {
					return err
				}
				return os.WriteFile(target, []byte("tampered-report\n"), 0o600)
			},
			want:    "tampered-report\n",
			recover: true,
			entries: 2,
		},
		{
			name: "target permissions widened",
			mutate: func(target string) error {
				return os.Chmod(target, 0o644)
			},
			want:    "complete-report\n",
			entries: 2,
		},
		{
			name: "target content size changes",
			mutate: func(target string) error {
				file, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					return err
				}
				if _, err := file.WriteString("x"); err != nil {
					_ = file.Close()
					return err
				}
				return file.Close()
			},
			want:    "complete-report\nx",
			entries: 2,
		},
		{
			name: "target gains unexpected hard link",
			mutate: func(target string) error {
				return os.Link(target, filepath.Join(filepath.Dir(target), "unexpected-link"))
			},
			want:    "complete-report\n",
			entries: 3,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := secureEvalOutputDirectory(t)
			target := filepath.Join(directory, "report.json")
			output, err := prepareEvalOutput(target)
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte("complete-report\n")
			err = output.writeWithOperations(payload, 1024, evalOutputOperations{
				afterPublish: func(string) error { return test.mutate(target) },
			})
			if !errors.Is(err, errEvalOutputIndeterminate) || !output.visible {
				t.Fatalf("write error/visible = %v/%v", err, output.visible)
			}
			if err := output.Close(); err != nil {
				t.Fatal(err)
			}
			if test.want != "" {
				visible, readErr := os.ReadFile(target)
				if readErr != nil || string(visible) != test.want {
					t.Fatalf("target/error = %q/%v", visible, readErr)
				}
			}
			if test.recover && !directoryContainsPrivatePayload(t, directory, payload) {
				t.Fatal("no recovery link retains the complete report")
			}
			assertEvalDirectoryEntryCount(t, directory, test.entries)
		})
	}
}

func TestEvalOutputRejectsTargetOwnerChangeAfterPublicationWhenPrivileged(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing file ownership safely requires an isolated privileged test process")
	}
	directory := secureEvalOutputDirectory(t)
	target := filepath.Join(directory, "report.json")
	output, err := prepareEvalOutput(target)
	if err != nil {
		t.Fatal(err)
	}
	err = output.writeWithOperations([]byte("complete-report\n"), 1024, evalOutputOperations{
		afterPublish: func(string) error { return os.Chown(target, 65534, -1) },
	})
	if !errors.Is(err, errEvalOutputIndeterminate) || !output.visible {
		t.Fatalf("write error/visible = %v/%v", err, output.visible)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	assertEvalDirectoryEntryCount(t, directory, 2)
}

func TestEvalOutputRejectsTemporaryReplacementAfterPublicationAndPreservesTarget(t *testing.T) {
	directory := secureEvalOutputDirectory(t)
	target := filepath.Join(directory, "report.json")
	output, err := prepareEvalOutput(target)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("complete-report\n")
	err = output.writeWithOperations(payload, 1024, evalOutputOperations{
		afterPublish: func(temporary string) error {
			temporaryPath := filepath.Join(directory, temporary)
			if err := os.Remove(temporaryPath); err != nil {
				return err
			}
			return os.WriteFile(temporaryPath, []byte("tampered-report\n"), 0o600)
		},
	})
	if !errors.Is(err, errEvalOutputIndeterminate) || !output.visible {
		t.Fatalf("write error/visible = %v/%v", err, output.visible)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	visible, err := os.ReadFile(target)
	if err != nil || string(visible) != string(payload) {
		t.Fatalf("target/error = %q/%v", visible, err)
	}
}

func TestEvalOutputSuccessfulPublicationLeavesOnePrivateReportLink(t *testing.T) {
	directory := secureEvalOutputDirectory(t)
	target := filepath.Join(directory, "report.json")
	output, err := prepareEvalOutput(target)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("complete-report\n")
	if err := output.Write(payload, 1024); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != int64(len(payload)) {
		t.Fatalf("target info/error = %#v/%v", info, err)
	}
	visible, err := os.ReadFile(target)
	if err != nil || string(visible) != string(payload) {
		t.Fatalf("target/error = %q/%v", visible, err)
	}
	assertNoEvalTemporaryFiles(t, directory)
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
	assertEvalDirectoryEntryCount(t, directory, 2)
}

func TestEvalOutputReopensTargetImmediatelyBeforeSuccess(t *testing.T) {
	directory := secureEvalOutputDirectory(t)
	target := filepath.Join(directory, "report.json")
	output, err := prepareEvalOutput(target)
	if err != nil {
		t.Fatal(err)
	}
	syncCalls := 0
	err = output.writeWithOperations([]byte("complete-report\n"), 1024, evalOutputOperations{
		syncDir: func(directory *os.File) error {
			syncCalls++
			if syncCalls != 2 {
				return directory.Sync()
			}
			if err := os.Remove(target); err != nil {
				return err
			}
			return os.WriteFile(target, []byte("tampered-report\n"), 0o600)
		},
	})
	if !errors.Is(err, errEvalOutputIndeterminate) || !output.visible || syncCalls != 2 {
		t.Fatalf("write error/visible/sync calls = %v/%v/%d", err, output.visible, syncCalls)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	visible, err := os.ReadFile(target)
	if err != nil || string(visible) != "tampered-report\n" {
		t.Fatalf("target/error = %q/%v", visible, err)
	}
	assertEvalDirectoryEntryCount(t, directory, 1)
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
				afterPublish: func(string) error {
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

func assertNoEvalTemporaryFiles(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".gocontext-eval-") {
			t.Fatalf("temporary report remains: %s", entry.Name())
		}
	}
}

func directoryContainsPrivatePayload(t *testing.T, directory string, want []byte) bool {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, statErr := os.Stat(path)
		payload, readErr := os.ReadFile(path)
		if statErr == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && readErr == nil && string(payload) == string(want) {
			return true
		}
	}
	return false
}

func assertEvalDirectoryEntryCount(t *testing.T, directory string, want int) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != want {
		t.Fatalf("directory entry count = %d, want %d", len(entries), want)
	}
}
