// Package filesystem implements repository ingestion from a local filesystem.
package filesystem

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/source"
)

// DefaultMaxFileSize limits memory use and avoids indexing generated bundles.
const DefaultMaxFileSize int64 = 1 << 20

var excludedDirectories = map[string]struct{}{
	".git":         {},
	".gocontext":   {},
	".next":        {},
	".venv":        {},
	"__pycache__":  {},
	"build":        {},
	"coverage":     {},
	"dist":         {},
	"node_modules": {},
	"vendor":       {},
	"venv":         {},
}

// Scanner reads supported source files without following directory symlinks.
type Scanner struct{}

// NewScanner creates a scanner with MVP-safe inclusion and exclusion rules.
func NewScanner() *Scanner {
	return &Scanner{}
}

// Scan discovers Python and TypeScript files below root.
func (s *Scanner) Scan(ctx context.Context, root string) ([]source.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if root == "" {
		return nil, fmt.Errorf("scan repository: root is empty")
	}

	repository, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open repository root: %w", err)
	}
	defer repository.Close()

	rootInfo, err := repository.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("inspect repository root: %w", err)
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("scan repository: root is not a directory")
	}

	var files []source.File
	err = fs.WalkDir(repository.FS(), ".", func(relativePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk %q: %w", relativePath, walkErr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if relativePath == "." {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			if _, excluded := excludedDirectories[entry.Name()]; excluded {
				return fs.SkipDir
			}
			return nil
		}

		language, supported := languageForPath(relativePath)
		if !supported {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect %q: %w", relativePath, err)
		}
		if !info.Mode().IsRegular() || info.Size() > DefaultMaxFileSize {
			return nil
		}

		file, err := repository.Open(relativePath)
		if err != nil {
			return fmt.Errorf("open %q: %w", relativePath, err)
		}
		content, overLimit, readErr := readLimited(file)
		closeErr := file.Close()
		if readErr != nil {
			return fmt.Errorf("read %q: %w", relativePath, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close %q: %w", relativePath, closeErr)
		}
		if overLimit || bytes.IndexByte(content, 0) >= 0 {
			return nil
		}

		reference := source.Reference{
			Path:      relativePath,
			StartLine: 1,
			EndLine:   lineCount(content),
		}
		if !reference.Valid() {
			return fmt.Errorf("scan %q: invalid repository-relative reference", relativePath)
		}

		files = append(files, source.File{
			Reference: reference,
			Language:  language,
			Content:   content,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan repository: %w", err)
	}

	return files, nil
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
