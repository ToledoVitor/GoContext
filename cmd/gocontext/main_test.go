package main

import (
	"bytes"
	"testing"
)

func TestRunVersion(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run([]string{"--version"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run() code = %d, want 0; stderr = %q", code, stderr.String())
	}
	if got, want := stdout.String(), "gocontext dev\n"; got != want {
		t.Fatalf("run() stdout = %q, want %q", got, want)
	}
}

func TestRunRejectsUnknownFlag(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run([]string{"--unknown"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("run() code = %d, want 2", code)
	}
}
