//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !hurd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris && !zos

package filesystem

import "os"

func inspectRepositoryEntry(_ repositoryHandle, _ string, entry os.DirEntry) (repositoryEntryMetadata, error) {
	info, err := entry.Info()
	if err != nil {
		return repositoryEntryMetadata{}, err
	}
	return repositoryEntryMetadata{mode: info.Mode(), size: info.Size(), identity: info}, nil
}

func sameRepositoryEntry(expected repositoryEntryMetadata, opened repositoryHandle) bool {
	want, ok := expected.identity.(os.FileInfo)
	got, err := opened.Stat()
	return ok && err == nil && os.SameFile(want, got)
}
