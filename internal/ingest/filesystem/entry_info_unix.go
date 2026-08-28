//go:build aix || android || darwin || dragonfly || freebsd || hurd || illumos || ios || linux || netbsd || openbsd || solaris || zos

package filesystem

import (
	"os"

	"golang.org/x/sys/unix"
)

type unixEntryIdentity struct {
	device uint64
	inode  uint64
}

func inspectRepositoryEntry(directory repositoryHandle, name string, _ os.DirEntry) (repositoryEntryMetadata, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return repositoryEntryMetadata{}, err
	}
	return repositoryEntryMetadata{
		mode: unixFileMode(uint32(stat.Mode)),
		size: stat.Size,
		identity: unixEntryIdentity{
			device: uint64(stat.Dev),
			inode:  uint64(stat.Ino),
		},
	}, nil
}

func sameRepositoryEntry(expected repositoryEntryMetadata, opened repositoryHandle) bool {
	want, ok := expected.identity.(unixEntryIdentity)
	if !ok {
		return false
	}
	var stat unix.Stat_t
	return unix.Fstat(int(opened.Fd()), &stat) == nil &&
		want.device == uint64(stat.Dev) && want.inode == uint64(stat.Ino)
}

func unixFileMode(mode uint32) os.FileMode {
	result := os.FileMode(mode & 0o777)
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
	case unix.S_IFDIR:
		result |= os.ModeDir
	case unix.S_IFLNK:
		result |= os.ModeSymlink
	case unix.S_IFIFO:
		result |= os.ModeNamedPipe
	case unix.S_IFSOCK:
		result |= os.ModeSocket
	case unix.S_IFBLK:
		result |= os.ModeDevice
	case unix.S_IFCHR:
		result |= os.ModeDevice | os.ModeCharDevice
	default:
		result |= os.ModeIrregular
	}
	if mode&unix.S_ISUID != 0 {
		result |= os.ModeSetuid
	}
	if mode&unix.S_ISGID != 0 {
		result |= os.ModeSetgid
	}
	if mode&unix.S_ISVTX != 0 {
		result |= os.ModeSticky
	}
	return result
}
