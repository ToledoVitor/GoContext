package ingest_test

import (
	"strings"
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

func TestUnsupportedExtensionBucketAllowsExpandedAggregateOnlyTaxonomy(t *testing.T) {
	for _, extension := range []string{
		".dart", ".m", ".mm", ".gradle", ".properties", ".lock", ".podspec", ".xcconfig", ".pbxproj",
		".plist", ".storyboard", ".xib", ".graphql", ".gql", ".proto", ".svelte", ".astro", ".pyi",
		".svg", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".bmp", ".ico", ".tif", ".tiff",
		".ttf", ".otf", ".woff", ".woff2", ".eot",
		".zip", ".tar", ".gz", ".tgz", ".bz2", ".xz", ".7z", ".rar", ".jar", ".war", ".aar", ".apk", ".ipa",
		".so", ".dylib", ".dll", ".a", ".lib", ".o", ".obj", ".exe", ".bin", ".wasm",
	} {
		name := "artifact" + strings.ToUpper(extension)
		if got := ingest.UnsupportedExtensionBucket(name); got != extension {
			t.Errorf("UnsupportedExtensionBucket(%q) = %q, want %q", name, got, extension)
		}
		if !ingest.SafeUnsupportedExtensionBucket(extension) {
			t.Errorf("SafeUnsupportedExtensionBucket(%q) = false", extension)
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
