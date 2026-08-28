//go:build !windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/ToledoVitor/GoContext/internal/ingest/filesystem"
	"golang.org/x/sys/unix"
)

var (
	errEvalOutput              = errors.New("evaluation output failed")
	errEvalOutputIndeterminate = errors.New("evaluation output durability indeterminate")
)

type evalOutput struct {
	parent  *evalDirectory
	target  string
	root    *filesystem.OpenedRoot
	visible bool
}

type evalOutputOperations struct {
	writeFile     func(*os.File, []byte) error
	syncFile      func(*os.File) error
	beforePublish func() error
	publish       func(*evalOutput, string) error
	afterPublish  func() error
	unlink        func(*evalOutput, string) error
	syncDir       func(*os.File) error
}

type evalFileOperations struct {
	afterOpenParent func() error
	root            *filesystem.OpenedRoot
}

type evalDirectory struct {
	file     *os.File
	path     string
	identity os.FileInfo
}

func prepareEvalOutput(targetPath string) (*evalOutput, error) {
	if !cleanAbsoluteEvalPath(targetPath) {
		return nil, errEvalOutput
	}
	parentPath := filepath.Dir(targetPath)
	parent, err := openPrivateEvalDirectory(parentPath)
	if err != nil {
		return nil, errEvalOutput
	}
	target := filepath.Base(targetPath)
	var stat unix.Stat_t
	err = unix.Fstatat(int(parent.file.Fd()), target, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil || !errors.Is(err, unix.ENOENT) {
		_ = parent.Close()
		return nil, errEvalOutput
	}
	return &evalOutput{parent: parent, target: target}, nil
}

func (output *evalOutput) requireOutsideRoot(root *filesystem.OpenedRoot) error {
	if output == nil || output.parent == nil || root == nil {
		return errEvalOutput
	}
	inside, err := output.parent.revalidate(root)
	if err != nil || inside {
		return errEvalOutput
	}
	output.root = root
	return nil
}

func (output *evalOutput) Write(payload []byte, maxBytes int64) error {
	return output.writeWithOperations(payload, maxBytes, evalOutputOperations{})
}

func (output *evalOutput) writeWithOperations(payload []byte, maxBytes int64, operations evalOutputOperations) error {
	if output == nil || output.parent == nil || output.parent.file == nil || output.visible || maxBytes <= 0 || int64(len(payload)) > maxBytes {
		return errEvalOutput
	}
	if err := output.revalidate(); err != nil {
		return errEvalOutput
	}
	if operations.writeFile == nil {
		operations.writeFile = writeAllEval
	}
	if operations.syncFile == nil {
		operations.syncFile = func(file *os.File) error { return file.Sync() }
	}
	if operations.publish == nil {
		operations.publish = func(output *evalOutput, temporary string) error {
			return unix.Linkat(int(output.parent.file.Fd()), temporary, int(output.parent.file.Fd()), output.target, 0)
		}
	}
	if operations.unlink == nil {
		operations.unlink = func(output *evalOutput, temporary string) error {
			return unix.Unlinkat(int(output.parent.file.Fd()), temporary, 0)
		}
	}
	if operations.syncDir == nil {
		operations.syncDir = func(directory *os.File) error { return directory.Sync() }
	}
	temporary, file, err := output.createTemporary()
	if err != nil {
		return errEvalOutput
	}
	cleanupTemporary := true
	defer func() {
		_ = file.Close()
		if cleanupTemporary {
			_ = unix.Unlinkat(int(output.parent.file.Fd()), temporary, 0)
		}
	}()
	if err := operations.writeFile(file, payload); err != nil || file.Chmod(0o600) != nil || operations.syncFile(file) != nil || file.Close() != nil {
		return errEvalOutput
	}
	if operations.beforePublish != nil {
		if err := operations.beforePublish(); err != nil {
			return errEvalOutput
		}
	}
	if err := output.revalidate(); err != nil {
		return errEvalOutput
	}
	if err := operations.publish(output, temporary); err != nil {
		return errEvalOutput
	}
	output.visible = true
	if operations.afterPublish != nil {
		if err := operations.afterPublish(); err != nil {
			return errEvalOutputIndeterminate
		}
	}
	if err := output.revalidate(); err != nil {
		return errEvalOutputIndeterminate
	}
	if err := operations.unlink(output, temporary); err != nil {
		cleanupTemporary = false
		return errEvalOutputIndeterminate
	}
	cleanupTemporary = false
	if err := output.revalidate(); err != nil {
		return errEvalOutputIndeterminate
	}
	if err := operations.syncDir(output.parent.file); err != nil {
		return errEvalOutputIndeterminate
	}
	if err := output.revalidate(); err != nil {
		return errEvalOutputIndeterminate
	}
	return nil
}

func (output *evalOutput) createTemporary() (string, *os.File, error) {
	for attempt := 0; attempt < 16; attempt++ {
		var random [16]byte
		if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
			return "", nil, err
		}
		name := ".gocontext-eval-" + hex.EncodeToString(random[:])
		descriptor, err := unix.Openat(int(output.parent.file.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err == nil {
			return name, os.NewFile(uintptr(descriptor), name), nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return "", nil, err
		}
	}
	return "", nil, errEvalOutput
}

func writeAllEval(file *os.File, payload []byte) error {
	for len(payload) > 0 {
		written, err := file.Write(payload)
		if err != nil || written <= 0 {
			return errEvalOutput
		}
		payload = payload[written:]
	}
	return nil
}

func (output *evalOutput) Close() error {
	if output == nil || output.parent == nil {
		return nil
	}
	err := output.parent.Close()
	output.parent = nil
	return err
}

func readPrivateEvalFileOutsideRoot(targetPath string, maxBytes int64, root *filesystem.OpenedRoot) ([]byte, error) {
	return readPrivateEvalFileWithOperations(targetPath, maxBytes, evalFileOperations{root: root})
}

func readPrivateEvalFileWithOperations(targetPath string, maxBytes int64, operations evalFileOperations) (payload []byte, resultErr error) {
	parentPath := filepath.Dir(targetPath)
	parent, err := openPrivateEvalDirectory(parentPath)
	if err != nil {
		return nil, errEvalChecklist
	}
	defer func() {
		if err := parent.Close(); err != nil && resultErr == nil {
			payload = nil
			resultErr = errEvalChecklist
		}
	}()
	if operations.afterOpenParent != nil {
		if err := operations.afterOpenParent(); err != nil {
			return nil, errEvalChecklist
		}
	}
	inside, err := parent.revalidate(operations.root)
	if err != nil {
		return nil, errEvalChecklist
	}
	if inside {
		return nil, errEvalChecklistLocation
	}
	descriptor, err := unix.Openat(int(parent.file.Fd()), filepath.Base(targetPath), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errEvalChecklist
	}
	file := os.NewFile(uintptr(descriptor), filepath.Base(targetPath))
	defer func() {
		if err := file.Close(); err != nil && resultErr == nil {
			payload = nil
			resultErr = errEvalChecklist
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maxBytes || !ownedByEffectiveUser(file) {
		return nil, errEvalChecklist
	}
	payload, err = io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(payload)) > maxBytes {
		return nil, errEvalChecklist
	}
	inside, err = parent.revalidate(operations.root)
	if err != nil {
		return nil, errEvalChecklist
	}
	if inside {
		return nil, errEvalChecklistLocation
	}
	return payload, nil
}

func openPrivateEvalDirectory(path string) (*evalDirectory, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return nil, errEvalOutput
	}
	descriptor, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errEvalOutput
	}
	directory := &evalDirectory{file: os.NewFile(uintptr(descriptor), path), path: path}
	info, err := directory.file.Stat()
	if err != nil {
		_ = directory.Close()
		return nil, errEvalOutput
	}
	directory.identity = info
	if _, err := directory.revalidate(nil); err != nil {
		_ = directory.Close()
		return nil, errEvalOutput
	}
	return directory, nil
}

func (directory *evalDirectory) revalidate(root *filesystem.OpenedRoot) (bool, error) {
	if directory == nil || directory.file == nil || directory.identity == nil {
		return false, errEvalOutput
	}
	canonical, err := filepath.EvalSymlinks(directory.path)
	pathInfo, pathErr := os.Lstat(directory.path)
	info, statErr := directory.file.Stat()
	if err != nil || canonical != directory.path || pathErr != nil || statErr != nil ||
		!info.IsDir() || !pathInfo.IsDir() || info.Mode().Perm() != 0o700 ||
		!ownedByEffectiveUser(directory.file) || !os.SameFile(directory.identity, info) || !os.SameFile(info, pathInfo) {
		return false, errEvalOutput
	}
	return validateEvalDirectoryAncestry(directory.file, root)
}

func (directory *evalDirectory) Close() error {
	if directory == nil || directory.file == nil {
		return nil
	}
	err := directory.file.Close()
	directory.file = nil
	return err
}

func (output *evalOutput) revalidate() error {
	inside, err := output.parent.revalidate(output.root)
	if err != nil || inside {
		return errEvalOutput
	}
	return nil
}

func validateEvalDirectoryAncestry(directory *os.File, root *filesystem.OpenedRoot) (bool, error) {
	if directory == nil {
		return false, errEvalOutput
	}
	descriptor, err := unix.Openat(int(directory.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	current := os.NewFile(uintptr(descriptor), ".")
	insideRoot := false
	for depth := 0; depth < 1024; depth++ {
		currentInfo, statErr := current.Stat()
		if statErr != nil {
			_ = current.Close()
			return false, statErr
		}
		if err := validateEvalDirectoryOwnerAndMode(current, depth == 0); err != nil {
			_ = current.Close()
			return false, err
		}
		if root != nil {
			sameRoot, compareErr := root.CompareIdentity(currentInfo)
			if compareErr != nil {
				_ = current.Close()
				return false, compareErr
			}
			if sameRoot {
				insideRoot = true
			}
		}
		parentDescriptor, openErr := unix.Openat(int(current.Fd()), "..", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			_ = current.Close()
			return false, openErr
		}
		parent := os.NewFile(uintptr(parentDescriptor), "..")
		parentInfo, parentStatErr := parent.Stat()
		if parentStatErr != nil {
			_ = current.Close()
			_ = parent.Close()
			return false, parentStatErr
		}
		atFilesystemRoot := os.SameFile(currentInfo, parentInfo)
		if closeErr := current.Close(); closeErr != nil {
			_ = parent.Close()
			return false, closeErr
		}
		if atFilesystemRoot {
			return insideRoot, parent.Close()
		}
		current = parent
	}
	_ = current.Close()
	return false, errEvalOutput
}

func validateEvalDirectoryOwnerAndMode(directory *os.File, leaf bool) error {
	var stat unix.Stat_t
	if unix.Fstat(int(directory.Fd()), &stat) != nil {
		return errEvalOutput
	}
	mode := uint32(stat.Mode)
	if leaf {
		if stat.Uid != uint32(os.Geteuid()) || mode&0o777 != 0o700 {
			return errEvalOutput
		}
		return nil
	}
	if stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
		return errEvalOutput
	}
	// Ancestors may be readable/traversable, but rename-capable ancestry is
	// limited to root-owned sticky system directories such as /tmp.
	if mode&0o022 != 0 && (stat.Uid != 0 || mode&unix.S_ISVTX == 0) {
		return errEvalOutput
	}
	return nil
}

func ownedByEffectiveUser(file *os.File) bool {
	var stat unix.Stat_t
	return unix.Fstat(int(file.Fd()), &stat) == nil && stat.Uid == uint32(os.Geteuid())
}
