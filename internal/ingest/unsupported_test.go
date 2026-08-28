package ingest_test

import (
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest"
)

func TestUnsupportedExtensionBucketUsesSanitizedAllowlist(t *testing.T) {
	tests := map[string]string{
		"README.MD":        ".md",
		"Makefile":         ingest.UnsupportedExtensionNone,
		"archive.private":  ingest.UnsupportedExtensionOther,
		"nested/name.JSON": ".json",
	}
	for name, want := range tests {
		if got := ingest.UnsupportedExtensionBucket(name); got != want {
			t.Errorf("UnsupportedExtensionBucket(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestSafeUnsupportedExtensionBucketRejectsUnsanitizedValues(t *testing.T) {
	for _, bucket := range []string{".md", ".json", ingest.UnsupportedExtensionOther, ingest.UnsupportedExtensionNone} {
		if !ingest.SafeUnsupportedExtensionBucket(bucket) {
			t.Errorf("SafeUnsupportedExtensionBucket(%q) = false", bucket)
		}
	}
	for _, bucket := range []string{".MD", ".private", "", "README.md"} {
		if ingest.SafeUnsupportedExtensionBucket(bucket) {
			t.Errorf("SafeUnsupportedExtensionBucket(%q) = true", bucket)
		}
	}
}
