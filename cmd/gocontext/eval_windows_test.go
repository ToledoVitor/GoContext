//go:build windows

package main

import (
	"bytes"
	"testing"
)

func TestRunEvalInventoryFailsClosedWhenOwnerOnlyOutputACLIsUnproved(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode := run([]string{
		"eval", "inventory",
		"--root", `C:\synthetic-root`,
		"--checklist", `C:\private\checklist.json`,
		"--output", `C:\private\report.json`,
		"--repository", "repo-a1",
	}, &stdout, &stderr)
	if exitCode != 1 || stdout.String() != "evaluation: no-go\n" || stderr.String() != "evaluation error: output\n" {
		t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
}
