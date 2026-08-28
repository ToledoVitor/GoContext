//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !hurd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris && !zos

package filesystem

import (
	"errors"
	"os"
)

func openNoFollow(repositoryHandle, string, bool) (*os.File, error) {
	return nil, errors.New("descriptor-relative no-follow open is unavailable")
}
