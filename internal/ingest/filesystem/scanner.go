// Package filesystem implements repository ingestion from a local filesystem.
package filesystem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/source"
)

// DefaultMaxFileSize limits memory use and avoids indexing generated bundles.
const DefaultMaxFileSize int64 = 1 << 20

type repositoryHandle interface {
	io.Reader
	ReadDir(n int) ([]os.DirEntry, error)
	Stat() (os.FileInfo, error)
	Close() error
	Fd() uintptr
	Name() string
}

type openPathFunc func(directory repositoryHandle, name string, wantDirectory bool) (repositoryHandle, error)
type inspectEntryFunc func(directory repositoryHandle, name string, entry os.DirEntry) (repositoryEntryMetadata, error)

type repositoryEntryMetadata struct {
	mode     os.FileMode
	size     int64
	identity any
}

func (info repositoryEntryMetadata) IsDir() bool       { return info.mode.IsDir() }
func (info repositoryEntryMetadata) Mode() os.FileMode { return info.mode }
func (info repositoryEntryMetadata) Size() int64       { return info.size }

// Scanner reads supported source files without following directory symlinks.
type Scanner struct {
	openPath     openPathFunc
	inspectEntry inspectEntryFunc
}

// OpenedRoot is an opaque, retained repository directory. It binds a scan to
// the exact filesystem object that passed preflight even if its path is later
// renamed or replaced.
type OpenedRoot struct {
	mu         sync.Mutex
	repository repositoryHandle
	identity   os.FileInfo
	used       bool
}

// NewScanner creates a scanner with the built-in, non-overridable policy.
func NewScanner() *Scanner {
	return &Scanner{}
}

// OpenRoot opens and validates a repository root without traversing it.
func OpenRoot(root string) (*OpenedRoot, error) {
	if root == "" {
		return nil, fmt.Errorf("scan repository: root is empty")
	}
	repository, err := openRootNoFollow(root)
	if err != nil {
		return nil, fmt.Errorf("scan repository: open root failed")
	}
	rootInfo, err := repository.Stat()
	if err != nil || !rootInfo.IsDir() {
		_ = repository.Close()
		if err != nil {
			return nil, fmt.Errorf("scan repository: inspect root failed")
		}
		return nil, fmt.Errorf("scan repository: root is not a directory")
	}
	return &OpenedRoot{repository: repository, identity: rootInfo}, nil
}

// Scan discovers safe Python and TypeScript files below an authorized root.
func (s *Scanner) Scan(ctx context.Context, root string) (ingest.ScanResult, error) {
	if err := ctx.Err(); err != nil {
		return ingest.ScanResult{}, err
	}
	opened, err := OpenRoot(root)
	if err != nil {
		return ingest.ScanResult{}, err
	}
	result, scanErr := s.ScanOpened(ctx, opened)
	closeErr := opened.Close()
	if scanErr != nil {
		return ingest.ScanResult{}, scanErr
	}
	if closeErr != nil {
		return ingest.ScanResult{}, fmt.Errorf("scan repository: close root failed")
	}
	return result, nil
}

// ScanOpened traverses an already-authorized root exactly once. The caller
// retains ownership of the handle and must close it.
func (s *Scanner) ScanOpened(ctx context.Context, opened *OpenedRoot) (ingest.ScanResult, error) {
	if err := ctx.Err(); err != nil {
		return ingest.ScanResult{}, err
	}
	if opened == nil {
		return ingest.ScanResult{}, fmt.Errorf("scan repository: opened root is invalid")
	}
	opened.mu.Lock()
	if opened.repository == nil || opened.used {
		opened.mu.Unlock()
		return ingest.ScanResult{}, fmt.Errorf("scan repository: opened root is invalid")
	}
	opened.used = true
	repository := opened.repository
	identity := opened.identity
	opened.mu.Unlock()
	currentInfo, err := repository.Stat()
	if err != nil || !currentInfo.IsDir() || !os.SameFile(identity, currentInfo) {
		return ingest.ScanResult{}, fmt.Errorf("scan repository: opened root changed")
	}
	result := ingest.ScanResult{
		PolicyVersion: ingest.ScanPolicyVersion,
		Report:        newScanReport(),
	}

	if err := s.scanDirectory(ctx, repository, ".", &result); err != nil {
		return ingest.ScanResult{}, fmt.Errorf("scan repository: %w", err)
	}
	return result, nil
}

// MatchesPath reports whether path currently names the retained root object.
func (root *OpenedRoot) MatchesPath(path string) bool {
	if root == nil {
		return false
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.repository == nil {
		return false
	}
	current, err := root.repository.Stat()
	pathInfo, pathErr := os.Lstat(path)
	return err == nil && pathErr == nil && current.IsDir() && pathInfo.IsDir() &&
		os.SameFile(root.identity, current) && os.SameFile(current, pathInfo)
}

// CompareIdentity compares info with the retained root and reports descriptor
// failures separately from an ordinary identity mismatch.
func (root *OpenedRoot) CompareIdentity(info os.FileInfo) (bool, error) {
	if root == nil || info == nil {
		return false, fmt.Errorf("scan repository: opened root is invalid")
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.repository == nil {
		return false, fmt.Errorf("scan repository: opened root is invalid")
	}
	current, err := root.repository.Stat()
	if err != nil || !current.IsDir() || !os.SameFile(root.identity, current) {
		return false, fmt.Errorf("scan repository: opened root changed")
	}
	return os.SameFile(current, info), nil
}

// Close releases the retained root descriptor.
func (root *OpenedRoot) Close() error {
	if root == nil {
		return nil
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.repository == nil {
		return nil
	}
	err := root.repository.Close()
	root.repository = nil
	return err
}

func (s *Scanner) scanDirectory(ctx context.Context, directory repositoryHandle, relativeDirectory string, result *ingest.ScanResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, readErr := directory.ReadDir(-1)
	if readErr != nil {
		return sanitizedPathError("read-directory", relativeDirectory)
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })

	if relativeDirectory != "." && containsGitMarker(entries) {
		addExclusion(&result.Report, ingest.ExclusionNestedRepository)
		return nil
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		relativePath := entry.Name()
		if relativeDirectory != "." {
			relativePath = path.Join(relativeDirectory, entry.Name())
		}
		if !validRelativePath(relativePath) {
			return sanitizedPathError("invalid-path", relativePath)
		}

		if reason, excluded := classifyPath(entry.Name(), entry.IsDir()); excluded {
			addExclusion(&result.Report, reason)
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			addExclusion(&result.Report, ingest.ExclusionSymlink)
			continue
		}

		entryInfo, err := s.inspectRepositoryPath(directory, entry.Name(), entry)
		if err != nil {
			return sanitizedPathError("inspect-entry", relativePath)
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			addExclusion(&result.Report, ingest.ExclusionSymlink)
			continue
		}
		isDirectory := entryInfo.IsDir()
		if isDirectory {
			childDirectory, err := s.openRepositoryPath(directory, entry.Name(), true)
			if err != nil {
				return sanitizedPathError("open-directory", relativePath)
			}
			openedInfo, statErr := childDirectory.Stat()
			if statErr != nil || !openedInfo.IsDir() || !sameRepositoryEntry(entryInfo, childDirectory) {
				_ = childDirectory.Close()
				return sanitizedPathError("directory-changed", relativePath)
			}
			scanErr := s.scanDirectory(ctx, childDirectory, relativePath, result)
			closeErr := childDirectory.Close()
			if scanErr != nil {
				return scanErr
			}
			if closeErr != nil {
				return sanitizedPathError("close-directory", relativePath)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}

		language, supported := languageForPath(relativePath)
		if !supported {
			var unsupportedBytes int64
			if entryInfo.Mode().IsRegular() {
				unsupportedBytes = entryInfo.Size()
			}
			addUnsupported(&result.Report, entry.Name(), unsupportedBytes)
			continue
		}

		if !entryInfo.Mode().IsRegular() {
			addExclusion(&result.Report, ingest.ExclusionNonRegular)
			continue
		}
		result.Report.EligibleFiles++
		result.Report.EligibleBytes += entryInfo.Size()
		if entryInfo.Size() > DefaultMaxFileSize {
			addExclusion(&result.Report, ingest.ExclusionTooLarge)
			continue
		}

		file, err := s.openRepositoryPath(directory, entry.Name(), false)
		if err != nil {
			return sanitizedPathError("open-file", relativePath)
		}
		openedInfo, statErr := file.Stat()
		if statErr != nil || !openedInfo.Mode().IsRegular() || !sameRepositoryEntry(entryInfo, file) {
			_ = file.Close()
			return sanitizedPathError("file-changed", relativePath)
		}
		content, overLimit, readErr := readLimited(file)
		closeErr := file.Close()
		if readErr != nil {
			return sanitizedPathError("read-file", relativePath)
		}
		if closeErr != nil {
			return sanitizedPathError("close-file", relativePath)
		}
		if overLimit {
			addExclusion(&result.Report, ingest.ExclusionTooLarge)
			continue
		}
		if bytes.IndexByte(content, 0) >= 0 {
			addExclusion(&result.Report, ingest.ExclusionBinary)
			continue
		}
		if reason, excluded := classifyContent(content); excluded {
			addExclusion(&result.Report, reason)
			continue
		}
		reference := source.Reference{Path: relativePath, StartLine: 1, EndLine: lineCount(content)}
		if !reference.Valid() {
			return sanitizedPathError("invalid-reference", relativePath)
		}
		result.Files = append(result.Files, source.File{Reference: reference, Language: language, Content: content})
		addIncluded(&result.Report, language, int64(len(content)))
	}
	return nil
}

func (s *Scanner) openRepositoryPath(directory repositoryHandle, name string, wantDirectory bool) (repositoryHandle, error) {
	if s.openPath != nil {
		return s.openPath(directory, name, wantDirectory)
	}
	return openNoFollow(directory, name, wantDirectory)
}

func (s *Scanner) inspectRepositoryPath(directory repositoryHandle, name string, entry os.DirEntry) (repositoryEntryMetadata, error) {
	if s.inspectEntry != nil {
		return s.inspectEntry(directory, name, entry)
	}
	return inspectRepositoryEntry(directory, name, entry)
}

func containsGitMarker(entries []os.DirEntry) bool {
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), ".git") {
			return true
		}
	}
	return false
}

func validRelativePath(relativePath string) bool {
	if !utf8.ValidString(relativePath) || relativePath == "" || relativePath == "." || path.IsAbs(relativePath) ||
		strings.ContainsRune(relativePath, '\\') || path.Clean(relativePath) != relativePath ||
		strings.HasPrefix(relativePath, "../") || relativePath == ".." {
		return false
	}
	for _, segment := range strings.Split(relativePath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, character := range segment {
			if unicode.IsControl(character) || unicode.In(character, unicode.Cf) {
				return false
			}
		}
	}
	return true
}

func sanitizedPathError(category, relativePath string) error {
	digest := sha256.Sum256([]byte(relativePath))
	return fmt.Errorf("%s [ref:%s]", category, hex.EncodeToString(digest[:4]))
}

func readLimited(reader io.Reader) ([]byte, bool, error) {
	content, err := io.ReadAll(io.LimitReader(reader, DefaultMaxFileSize+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(content)) > DefaultMaxFileSize {
		return nil, true, nil
	}
	return content, false, nil
}

func languageForPath(filePath string) (source.Language, bool) {
	switch strings.ToLower(path.Ext(filePath)) {
	case ".py":
		return source.LanguagePython, true
	case ".ts", ".tsx":
		return source.LanguageTypeScript, true
	default:
		return source.LanguageUnknown, false
	}
}

func lineCount(content []byte) int {
	if len(content) == 0 {
		return 1
	}
	lines := bytes.Count(content, []byte{'\n'})
	if content[len(content)-1] != '\n' {
		lines++
	}
	return lines
}

var _ ingest.Scanner = (*Scanner)(nil)
