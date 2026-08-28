//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !hurd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris && !zos

package filesystem_test

import "testing"

func makeSpecialFile(t *testing.T, _ string) {
	t.Helper()
	t.Skip("special-file fixture unavailable on this platform")
}
