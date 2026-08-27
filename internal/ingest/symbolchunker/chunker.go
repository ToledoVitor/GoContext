// Package symbolchunker creates retrievable chunks from source declarations.
package symbolchunker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/source"
)

var ErrInvalidInput = errors.New("invalid chunker input")

// Chunker groups source lines by top-level symbol boundaries.
type Chunker struct{}

// NewChunker creates a symbol-oriented chunker.
func NewChunker() *Chunker {
	return &Chunker{}
}

// Chunk returns one chunk per symbol, or one file chunk when symbols are absent.
func (c *Chunker) Chunk(ctx context.Context, file source.File, symbols []source.Symbol) ([]source.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !file.Reference.Valid() {
		return nil, ErrInvalidInput
	}

	lines := contentLines(file.Content)
	contentEndLine := file.Reference.StartLine + len(lines) - 1
	if contentEndLine != file.Reference.EndLine {
		return nil, ErrInvalidInput
	}

	if len(symbols) == 0 {
		return fallbackChunk(file, lines), nil
	}
	if !validSymbols(file.Reference, contentEndLine, symbols) {
		return nil, ErrInvalidInput
	}

	chunks := make([]source.Chunk, 0, len(symbols))
	for index, symbol := range symbols {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		startLine := symbol.Reference.StartLine
		endLine := contentEndLine
		if index+1 < len(symbols) {
			endLine = symbols[index+1].Reference.StartLine - 1
		}

		startIndex := startLine - file.Reference.StartLine
		endIndex := endLine - file.Reference.StartLine
		endIndex = trimTrailingBlankLines(lines, startIndex, endIndex)
		if endIndex < startIndex || strings.TrimSpace(lines[startIndex]) == "" {
			return nil, ErrInvalidInput
		}

		endLine = file.Reference.StartLine + endIndex
		text := strings.Join(lines[startIndex:endIndex+1], "\n")
		chunks = append(chunks, newChunk(file, symbol.Name, startLine, endLine, text))
	}

	return chunks, nil
}

func fallbackChunk(file source.File, lines []string) []source.Chunk {
	startIndex := 0
	for startIndex < len(lines) && strings.TrimSpace(lines[startIndex]) == "" {
		startIndex++
	}
	if startIndex == len(lines) {
		return nil
	}

	endIndex := trimTrailingBlankLines(lines, startIndex, len(lines)-1)
	startLine := file.Reference.StartLine + startIndex
	endLine := file.Reference.StartLine + endIndex
	text := strings.Join(lines[startIndex:endIndex+1], "\n")
	return []source.Chunk{newChunk(file, "", startLine, endLine, text)}
}

func validSymbols(fileReference source.Reference, contentEndLine int, symbols []source.Symbol) bool {
	previousStart := fileReference.StartLine - 1
	for _, symbol := range symbols {
		if symbol.Name == "" ||
			!symbol.Reference.Valid() ||
			symbol.Reference.Path != fileReference.Path ||
			symbol.Reference.StartLine <= previousStart ||
			symbol.Reference.StartLine < fileReference.StartLine ||
			symbol.Reference.EndLine > contentEndLine {
			return false
		}
		previousStart = symbol.Reference.StartLine
	}
	return true
}

func contentLines(content []byte) []string {
	normalized := strings.ReplaceAll(string(content), "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func trimTrailingBlankLines(lines []string, start, end int) int {
	for end >= start && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	return end
}

func newChunk(file source.File, symbolName string, startLine, endLine int, text string) source.Chunk {
	chunk := source.Chunk{
		Text:       text,
		Language:   file.Language,
		SymbolName: symbolName,
		Reference: source.Reference{
			Path:      file.Reference.Path,
			StartLine: startLine,
			EndLine:   endLine,
		},
	}
	chunk.ID = chunkID(chunk)
	return chunk
}

func chunkID(chunk source.Chunk) string {
	digest := sha256.New()
	digest.Write([]byte("gocontext:chunk:v1\x00"))
	digest.Write([]byte(chunk.Reference.Path))
	digest.Write([]byte{'\x00'})
	digest.Write([]byte(strconv.Itoa(chunk.Reference.StartLine)))
	digest.Write([]byte{'\x00'})
	digest.Write([]byte(strconv.Itoa(chunk.Reference.EndLine)))
	digest.Write([]byte{'\x00'})
	digest.Write([]byte(chunk.Language))
	digest.Write([]byte{'\x00'})
	digest.Write([]byte(chunk.SymbolName))
	digest.Write([]byte{'\x00'})
	digest.Write([]byte(chunk.Text))
	return hex.EncodeToString(digest.Sum(nil))
}

var _ ingest.Chunker = (*Chunker)(nil)
