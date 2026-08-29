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
	afterPublish  func(string) error
	unlink        func(*evalOutput, string) error
	syncDir       func(*os.File) error
}

type evalReportIdentity struct {
	info os.FileInfo
	size int64
}

type evalFileOperations struct {
	afterOpenParent func() error
	beforeOpenFile  func() error
	afterOpenFile   func() error
	afterRead       func() error
	policy          evalPrivateFilePolicy
	root            *filesystem.OpenedRoot
}

type evalPrivateFileIdentity struct {
	stat unix.Stat_t
}

type evalPrivateFilePolicy struct {
	exactMode         os.FileMode
	requireSingleLink bool
}

var evalGoldFilePolicy = evalPrivateFilePolicy{exactMode: 0o600, requireSingleLink: true}

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
		if cleanupTemporary && !output.visible {
			_ = unix.Unlinkat(int(output.parent.file.Fd()), temporary, 0)
		}
	}()
	if err := operations.writeFile(file, payload); err != nil || file.Chmod(0o600) != nil || operations.syncFile(file) != nil {
		return errEvalOutput
	}
	report, err := captureEvalReportIdentity(file, int64(len(payload)))
	if err != nil || file.Close() != nil {
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
		if err := operations.afterPublish(temporary); err != nil {
			return errEvalOutputIndeterminate
		}
	}
	if err := output.revalidate(); err != nil {
		return errEvalOutputIndeterminate
	}
	if err := output.validateReportLink(output.target, report, 2); err != nil {
		return errEvalOutputIndeterminate
	}
	if err := output.validateReportLink(temporary, report, 2); err != nil {
		return errEvalOutputIndeterminate
	}
	// Make the target link durable before considering removal of the only
	// descriptor-relative recovery name.
	if err := operations.syncDir(output.parent.file); err != nil {
		return errEvalOutputIndeterminate
	}
	if err := output.revalidate(); err != nil {
		return errEvalOutputIndeterminate
	}
	if err := output.validateReportLink(output.target, report, 2); err != nil {
		return errEvalOutputIndeterminate
	}
	if err := output.validateReportLink(temporary, report, 2); err != nil {
		return errEvalOutputIndeterminate
	}
	if err := operations.unlink(output, temporary); err != nil {
		return errEvalOutputIndeterminate
	}
	cleanupTemporary = false
	if err := output.revalidate(); err != nil || output.validateReportLink(output.target, report, 1) != nil {
		return errEvalOutputIndeterminate
	}
	if err := operations.syncDir(output.parent.file); err != nil {
		return errEvalOutputIndeterminate
	}
	// This final descriptor-relative reopen is the success boundary. It
	// protects cooperating-process operation, but cannot prevent a hostile
	// same-UID process from racing a mutation immediately after the check.
	if err := output.revalidate(); err != nil || output.validateReportLink(output.target, report, 1) != nil {
		return errEvalOutputIndeterminate
	}
	return nil
}

func captureEvalReportIdentity(file *os.File, size int64) (*evalReportIdentity, error) {
	if file == nil {
		return nil, errEvalOutput
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errEvalOutput
	}
	report := &evalReportIdentity{info: info, size: size}
	if err := validateOpenEvalReport(file, report, 1); err != nil {
		return nil, err
	}
	return report, nil
}

func (output *evalOutput) validateReportLink(name string, expected *evalReportIdentity, links uint64) error {
	if output == nil || output.parent == nil || output.parent.file == nil || name == "" || expected == nil || expected.info == nil {
		return errEvalOutput
	}
	descriptor, err := unix.Openat(int(output.parent.file.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errEvalOutput
	}
	file := os.NewFile(uintptr(descriptor), name)
	validationErr := validateOpenEvalReport(file, expected, links)
	closeErr := file.Close()
	if validationErr != nil || closeErr != nil {
		return errEvalOutput
	}
	return nil
}

func validateOpenEvalReport(file *os.File, expected *evalReportIdentity, links uint64) error {
	if file == nil || expected == nil || expected.info == nil || links == 0 {
		return errEvalOutput
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != expected.size || !os.SameFile(info, expected.info) {
		return errEvalOutput
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return errEvalOutput
	}
	if uint32(stat.Mode)&0o7777 != 0o600 || stat.Uid != uint32(os.Geteuid()) || uint64(stat.Nlink) != links {
		return errEvalOutput
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

func readPrivateEvalGoldFileOutsideRoot(targetPath string, maxBytes int64, root *filesystem.OpenedRoot) ([]byte, error) {
	return readPrivateEvalFileWithOperations(targetPath, maxBytes, evalFileOperations{policy: evalGoldFilePolicy, root: root})
}

func readPrivateEvalFileWithOperations(targetPath string, maxBytes int64, operations evalFileOperations) (payload []byte, resultErr error) {
	if maxBytes <= 0 {
		return nil, errEvalChecklist
	}
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
	name := filepath.Base(targetPath)
	identity, err := capturePrivateEvalFilename(parent.file, name, maxBytes, operations.policy)
	if err != nil {
		return nil, errEvalChecklist
	}
	if operations.beforeOpenFile != nil {
		if err := operations.beforeOpenFile(); err != nil {
			return nil, errEvalChecklist
		}
	}
	descriptor, err := unix.Openat(int(parent.file.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errEvalChecklist
	}
	file := os.NewFile(uintptr(descriptor), name)
	defer func() {
		if err := file.Close(); err != nil && resultErr == nil {
			payload = nil
			resultErr = errEvalChecklist
		}
	}()
	if validatePrivateEvalDescriptor(file, identity, maxBytes, operations.policy) != nil ||
		validatePrivateEvalFilename(parent.file, name, identity, maxBytes, operations.policy) != nil {
		return nil, errEvalChecklist
	}
	if operations.afterOpenFile != nil {
		if err := operations.afterOpenFile(); err != nil {
			return nil, errEvalChecklist
		}
	}
	if validatePrivateEvalDescriptor(file, identity, maxBytes, operations.policy) != nil ||
		validatePrivateEvalFilename(parent.file, name, identity, maxBytes, operations.policy) != nil {
		return nil, errEvalChecklist
	}
	payload, err = io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(payload)) > maxBytes || int64(len(payload)) != identity.stat.Size {
		return nil, errEvalChecklist
	}
	if operations.afterRead != nil {
		if err := operations.afterRead(); err != nil {
			return nil, errEvalChecklist
		}
	}
	if validatePrivateEvalDescriptor(file, identity, maxBytes, operations.policy) != nil ||
		validatePrivateEvalFilename(parent.file, name, identity, maxBytes, operations.policy) != nil {
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

func capturePrivateEvalFilename(parent *os.File, name string, maxBytes int64, policy evalPrivateFilePolicy) (*evalPrivateFileIdentity, error) {
	if parent == nil || name == "" {
		return nil, errEvalChecklist
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		!validPrivateEvalStat(&stat, maxBytes, policy) {
		return nil, errEvalChecklist
	}
	return &evalPrivateFileIdentity{stat: stat}, nil
}

func validatePrivateEvalFilename(parent *os.File, name string, expected *evalPrivateFileIdentity, maxBytes int64, policy evalPrivateFilePolicy) error {
	if expected == nil {
		return errEvalChecklist
	}
	current, err := capturePrivateEvalFilename(parent, name, maxBytes, policy)
	if err != nil || !samePrivateEvalStat(&expected.stat, &current.stat) {
		return errEvalChecklist
	}
	return nil
}

func validatePrivateEvalDescriptor(file *os.File, expected *evalPrivateFileIdentity, maxBytes int64, policy evalPrivateFilePolicy) error {
	if file == nil || expected == nil {
		return errEvalChecklist
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil || !validPrivateEvalStat(&stat, maxBytes, policy) ||
		!samePrivateEvalStat(&expected.stat, &stat) {
		return errEvalChecklist
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !policy.validMode(info.Mode()) || info.Size() != stat.Size {
		return errEvalChecklist
	}
	return nil
}

func validPrivateEvalStat(stat *unix.Stat_t, maxBytes int64, policy evalPrivateFilePolicy) bool {
	return stat != nil && maxBytes > 0 && uint32(stat.Mode)&unix.S_IFMT == unix.S_IFREG &&
		policy.validRawMode(uint32(stat.Mode)&0o7777) && stat.Uid == uint32(os.Geteuid()) &&
		uint64(stat.Nlink) >= 1 && (!policy.requireSingleLink || uint64(stat.Nlink) == 1) &&
		stat.Size >= 0 && stat.Size <= maxBytes
}

func (policy evalPrivateFilePolicy) validRawMode(mode uint32) bool {
	if policy.exactMode != 0 {
		return mode == uint32(policy.exactMode)&0o7777
	}
	return policy.validMode(os.FileMode(mode))
}

func (policy evalPrivateFilePolicy) validMode(mode os.FileMode) bool {
	permissions := mode.Perm()
	if policy.exactMode != 0 {
		return permissions == policy.exactMode
	}
	return permissions&0o077 == 0 && permissions&0o400 != 0
}

func samePrivateEvalStat(left, right *unix.Stat_t) bool {
	return left != nil && right != nil && left.Dev == right.Dev && left.Ino == right.Ino &&
		left.Mode == right.Mode && left.Uid == right.Uid && left.Nlink == right.Nlink && left.Size == right.Size
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
