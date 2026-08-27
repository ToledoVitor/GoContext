// Package source contains source-code facts that retain their provenance
// throughout ingestion, search, and answer generation.
package source

import (
	"path"
	"strings"
)

// Language identifies a source language supported by the ingestion pipeline.
type Language string

const (
	LanguageUnknown    Language = "unknown"
	LanguagePython     Language = "python"
	LanguageTypeScript Language = "typescript"
)

// Reference identifies an inclusive line range in a normalized,
// repository-relative file path.
type Reference struct {
	Path      string
	StartLine int
	EndLine   int
}

// Valid reports whether a reference can be presented as a citation.
func (r Reference) Valid() bool {
	cleanPath := path.Clean(r.Path)
	pathIsRelative := r.Path != "" &&
		!path.IsAbs(r.Path) &&
		!strings.ContainsRune(r.Path, '\\') &&
		cleanPath == r.Path &&
		cleanPath != "." &&
		cleanPath != ".." &&
		!strings.HasPrefix(cleanPath, "../")

	return pathIsRelative && r.StartLine > 0 && r.EndLine >= r.StartLine
}

// File is scanner output. Content is transient and must not be logged.
type File struct {
	Reference Reference
	Language  Language
	Content   []byte
}

// Symbol is a parser-discovered declaration inside a file.
type Symbol struct {
	Name      string
	Kind      string
	Signature string
	Reference Reference
}

// Chunk is the smallest independently retrievable source unit.
type Chunk struct {
	ID         string
	Text       string
	Language   Language
	SymbolName string
	Reference  Reference
}

// Citation connects answer text back to a retrieved chunk and source range.
type Citation struct {
	ChunkID   string
	Reference Reference
}
