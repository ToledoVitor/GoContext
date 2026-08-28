//go:build aix || android || darwin || dragonfly || freebsd || hurd || illumos || ios || linux || netbsd || openbsd || solaris || zos

package filesystem_test

import (
	"testing"

	"golang.org/x/sys/unix"
)

func makeSpecialFile(t *testing.T, filePath string) {
	t.Helper()
	if err := unix.Mkfifo(filePath, 0o600); err != nil {
		t.Skipf("named pipes unavailable: %v", err)
	}
}
