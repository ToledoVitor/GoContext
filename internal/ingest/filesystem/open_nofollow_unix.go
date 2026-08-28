//go:build aix || android || darwin || dragonfly || freebsd || hurd || illumos || ios || linux || netbsd || openbsd || solaris || zos

package filesystem

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openNoFollow(directory repositoryHandle, name string, wantDirectory bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if wantDirectory {
		flags |= unix.O_DIRECTORY
	}
	fileDescriptor, err := unix.Openat(int(directory.Fd()), name, flags, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fileDescriptor), filepath.Join(directory.Name(), name)), nil
}

func openRootNoFollow(root string) (*os.File, error) {
	fileDescriptor, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fileDescriptor), root), nil
}
