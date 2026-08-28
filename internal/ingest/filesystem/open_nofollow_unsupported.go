//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !hurd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris && !zos

package filesystem

import (
	"errors"
	"os"
)

func openNoFollow(repositoryHandle, string, bool) (*os.File, error) {
	return nil, errors.New("descriptor-relative no-follow open is unavailable")
}

func openRootNoFollow(root string) (*os.File, error) {
	// Preserve the pre-secure-scanner behavior for ordinary indexing on
	// unsupported platforms. Child opens still fail closed because their
	// descriptor-relative no-follow semantics cannot be established.
	return os.Open(root)
}
