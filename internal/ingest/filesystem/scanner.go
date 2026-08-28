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

// Scanner reads supported source files without following directory symlinks.
type Scanner struct {
	openPath openPathFunc
}

// NewScanner creates a scanner with the built-in, non-overridable policy.
func NewScanner() *Scanner {
	return &Scanner{}
}

// Scan discovers safe Python and TypeScript files below an authorized root.
func (s *Scanner) Scan(ctx context.Context, root string) (ingest.ScanResult, error) {
	result := ingest.ScanResult{
		PolicyVersion: ingest.ScanPolicyVersion,
		Report:        newScanReport(),
	}
	if err := ctx.Err(); err != nil {
		return ingest.ScanResult{}, err
	}
	if root == "" {
		return ingest.ScanResult{}, fmt.Errorf("scan repository: root is empty")
	}

	repository, err := os.Open(root)
	if err != nil {
		return ingest.ScanResult{}, fmt.Errorf("scan repository: open root failed")
	}
	defer repository.Close()

	rootInfo, err := repository.Stat()
	if err != nil {
		return ingest.ScanResult{}, fmt.Errorf("scan repository: inspect root failed")
	}
	if !rootInfo.IsDir() {
		return ingest.ScanResult{}, fmt.Errorf("scan repository: root is not a directory")
	}

	if err := s.scanDirectory(ctx, repository, ".", &result); err != nil {
		return ingest.ScanResult{}, fmt.Errorf("scan repository: %w", err)
	}
	return result, nil
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
		if entry.IsDir() {
			entryInfo, err := entry.Info()
			if err != nil {
				return sanitizedPathError("inspect-directory", relativePath)
			}
			childDirectory, err := s.openRepositoryPath(directory, entry.Name(), true)
			if err != nil {
				return sanitizedPathError("open-directory", relativePath)
			}
			openedInfo, statErr := childDirectory.Stat()
			if statErr != nil || !openedInfo.IsDir() || !os.SameFile(entryInfo, openedInfo) {
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
			addUnsupported(&result.Report, entry.Name())
			continue
		}

		info, err := entry.Info()
		if err != nil {
			return sanitizedPathError("inspect-file", relativePath)
		}
		if !info.Mode().IsRegular() {
			addExclusion(&result.Report, ingest.ExclusionNonRegular)
			continue
		}
		result.Report.EligibleFiles++
		result.Report.EligibleBytes += info.Size()
		if info.Size() > DefaultMaxFileSize {
			addExclusion(&result.Report, ingest.ExclusionTooLarge)
			continue
		}

		file, err := s.openRepositoryPath(directory, entry.Name(), false)
		if err != nil {
			return sanitizedPathError("open-file", relativePath)
		}
		openedInfo, statErr := file.Stat()
		if statErr != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
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
