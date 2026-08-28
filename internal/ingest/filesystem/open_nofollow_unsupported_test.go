//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !hurd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris && !zos

package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOrdinaryScanPreservesLegacyRootOpenOnUnsupportedPlatform(t *testing.T) {
	root := t.TempDir()
	if _, err := NewScanner().Scan(context.Background(), root); err != nil {
		t.Fatalf("empty-root Scan() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "source.py"), []byte("print('x')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewScanner().Scan(context.Background(), root); err == nil {
		t.Fatal("source Scan() error = nil; descriptor-relative child open must remain fail-closed")
	}
}

func TestOrdinaryScanPreservesLegacyRootSymlinkFollowOnUnsupportedPlatform(t *testing.T) {
	realRoot := t.TempDir()
	alias := filepath.Join(t.TempDir(), "root-alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Skipf("root symlink unavailable: %v", err)
	}
	if _, err := NewScanner().Scan(context.Background(), alias); err != nil {
		t.Fatalf("empty symlink-root Scan() error = %v", err)
	}
}
