//go:build !windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var (
	errEvalOutput              = errors.New("evaluation output failed")
	errEvalOutputIndeterminate = errors.New("evaluation output durability indeterminate")
)

type evalOutput struct {
	parent     *os.File
	parentPath string
	target     string
	visible    bool
}

type evalOutputOperations struct {
	writeFile func(*os.File, []byte) error
	syncFile  func(*os.File) error
	publish   func(*evalOutput, string) error
	unlink    func(*evalOutput, string) error
	syncDir   func(*os.File) error
}

func prepareEvalOutput(targetPath string) (*evalOutput, error) {
	if !cleanAbsoluteEvalPath(targetPath) {
		return nil, errEvalOutput
	}
	parentPath := filepath.Dir(targetPath)
	canonicalParent, err := filepath.EvalSymlinks(parentPath)
	if err != nil || canonicalParent != parentPath {
		return nil, errEvalOutput
	}
	parent, err := os.Open(parentPath)
	if err != nil {
		return nil, errEvalOutput
	}
	info, err := parent.Stat()
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !ownedByEffectiveUser(parent) {
		_ = parent.Close()
		return nil, errEvalOutput
	}
	target := filepath.Base(targetPath)
	var stat unix.Stat_t
	err = unix.Fstatat(int(parent.Fd()), target, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil || !errors.Is(err, unix.ENOENT) {
		_ = parent.Close()
		return nil, errEvalOutput
	}
	return &evalOutput{parent: parent, parentPath: canonicalParent, target: target}, nil
}

func (output *evalOutput) absolutePath() string {
	return filepath.Join(output.parentPath, output.target)
}

func (output *evalOutput) Write(payload []byte, maxBytes int64) error {
	return output.writeWithOperations(payload, maxBytes, evalOutputOperations{})
}

func (output *evalOutput) writeWithOperations(payload []byte, maxBytes int64, operations evalOutputOperations) error {
	if output == nil || output.parent == nil || output.visible || maxBytes <= 0 || int64(len(payload)) > maxBytes {
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
			return unix.Linkat(int(output.parent.Fd()), temporary, int(output.parent.Fd()), output.target, 0)
		}
	}
	if operations.unlink == nil {
		operations.unlink = func(output *evalOutput, temporary string) error {
			return unix.Unlinkat(int(output.parent.Fd()), temporary, 0)
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
			_ = unix.Unlinkat(int(output.parent.Fd()), temporary, 0)
		}
	}()
	if err := operations.writeFile(file, payload); err != nil || file.Chmod(0o600) != nil || operations.syncFile(file) != nil || file.Close() != nil {
		return errEvalOutput
	}
	if err := operations.publish(output, temporary); err != nil {
		return errEvalOutput
	}
	output.visible = true
	if err := operations.unlink(output, temporary); err != nil {
		cleanupTemporary = false
		return errEvalOutputIndeterminate
	}
	cleanupTemporary = false
	if err := operations.syncDir(output.parent); err != nil {
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
		descriptor, err := unix.Openat(int(output.parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
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

func readPrivateEvalFile(targetPath string, maxBytes int64) ([]byte, error) {
	parentPath := filepath.Dir(targetPath)
	canonicalParent, err := filepath.EvalSymlinks(parentPath)
	if err != nil || canonicalParent != parentPath {
		return nil, errEvalChecklist
	}
	parent, err := os.Open(parentPath)
	if err != nil {
		return nil, errEvalChecklist
	}
	defer parent.Close()
	parentInfo, err := parent.Stat()
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode().Perm() != 0o700 || !ownedByEffectiveUser(parent) {
		return nil, errEvalChecklist
	}
	descriptor, err := unix.Openat(int(parent.Fd()), filepath.Base(targetPath), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errEvalChecklist
	}
	file := os.NewFile(uintptr(descriptor), filepath.Base(targetPath))
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maxBytes || !ownedByEffectiveUser(file) {
		return nil, errEvalChecklist
	}
	payload, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(payload)) > maxBytes {
		return nil, errEvalChecklist
	}
	return payload, nil
}

func ownedByEffectiveUser(file *os.File) bool {
	var stat unix.Stat_t
	return unix.Fstat(int(file.Fd()), &stat) == nil && stat.Uid == uint32(os.Geteuid())
}
