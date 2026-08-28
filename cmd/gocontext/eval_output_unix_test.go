//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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
