//go:build aix || android || darwin || dragonfly || freebsd || hurd || illumos || ios || linux || netbsd || openbsd || solaris || zos

package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScannerRejectsRepositoryRootSymlinkWhenNoFollowIsAvailable(t *testing.T) {
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
