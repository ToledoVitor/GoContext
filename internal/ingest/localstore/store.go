// Package localstore persists repository chunk snapshots on local disk.
package localstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/source"
)

const (
	snapshotVersion = 1
	maxSnapshotSize = 64 << 20
)

var (
	ErrNotFound            = errors.New("repository snapshot not found")
	ErrInvalidRepositoryID = errors.New("invalid repository ID")
	ErrInvalidChunk        = errors.New("invalid chunk")
	ErrSnapshotTooLarge    = errors.New("repository snapshot too large")
)

// Store keeps one versioned JSON snapshot per repository.
type Store struct {
	directory string
}

type snapshot struct {
	Version      int            `json:"version"`
	RepositoryID string         `json:"repository_id"`
	Chunks       []source.Chunk `json:"chunks"`
}

// NewStore creates or opens a private local snapshot directory.
func NewStore(directory string) (*Store, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, fmt.Errorf("create local store: directory is empty")
	}

	absoluteDirectory, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve local store directory: %w", err)
	}
	if err := os.MkdirAll(absoluteDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create local store directory: %w", err)
	}
	info, err := os.Stat(absoluteDirectory)
	if err != nil {
		return nil, fmt.Errorf("inspect local store directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("create local store: path is not a directory")
	}
	canonicalDirectory, err := filepath.EvalSymlinks(absoluteDirectory)
	if err != nil {
		return nil, fmt.Errorf("resolve local store symlinks: %w", err)
	}

	return &Store{directory: canonicalDirectory}, nil
}

// Replace atomically replaces chunks belonging to one repository snapshot.
func (s *Store) Replace(ctx context.Context, repositoryID string, chunks []source.Chunk) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("replace repository snapshot: %w", err)
	}
	if err := validateRepositoryID(repositoryID); err != nil {
		return fmt.Errorf("replace repository snapshot: %w", err)
	}
	targetName := snapshotName(repositoryID)
	if err := validateChunks(chunks); err != nil {
		return fmt.Errorf("replace repository snapshot %q: %w", targetName, err)
	}

	payload, err := json.Marshal(snapshot{
		Version:      snapshotVersion,
		RepositoryID: repositoryID,
		Chunks:       chunks,
	})
	if err != nil {
		return fmt.Errorf("encode repository snapshot: %w", err)
	}
	if len(payload) > maxSnapshotSize {
		return fmt.Errorf("replace repository snapshot %q: %w", targetName, ErrSnapshotTooLarge)
	}

	root, err := os.OpenRoot(s.directory)
	if err != nil {
		return fmt.Errorf("open local store: %w", err)
	}
	defer root.Close()

	temporaryName, err := temporarySnapshotName(targetName)
	if err != nil {
		return fmt.Errorf("create snapshot name: %w", err)
	}
	file, err := root.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary snapshot: %w", err)
	}
	keepTemporary := true
	defer func() {
		if keepTemporary {
			_ = root.Remove(temporaryName)
		}
	}()

	_, writeErr := io.Copy(file, bytes.NewReader(payload))
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write repository snapshot: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close repository snapshot: %w", closeErr)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("replace repository snapshot %q: %w", targetName, err)
	}
	if err := os.Rename(
		filepath.Join(s.directory, temporaryName),
		filepath.Join(s.directory, targetName),
	); err != nil {
		return fmt.Errorf("replace repository snapshot: %w", err)
	}
	keepTemporary = false

	if err := syncDirectory(root); err != nil {
		return fmt.Errorf("sync local store directory: %w", err)
	}
	return nil
}

// Load reads the current snapshot for a repository.
func (s *Store) Load(ctx context.Context, repositoryID string) ([]source.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("load repository snapshot: %w", err)
	}
	if err := validateRepositoryID(repositoryID); err != nil {
		return nil, fmt.Errorf("load repository snapshot: %w", err)
	}
	targetName := snapshotName(repositoryID)

	root, err := os.OpenRoot(s.directory)
	if err != nil {
		return nil, fmt.Errorf("open local store: %w", err)
	}
	defer root.Close()

	file, err := root.Open(targetName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("load repository snapshot %q: %w", targetName, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("open repository snapshot: %w", err)
	}
	payload, overLimit, readErr := readSnapshot(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read repository snapshot: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close repository snapshot: %w", closeErr)
	}
	if overLimit {
		return nil, fmt.Errorf("load repository snapshot %q: %w", targetName, ErrSnapshotTooLarge)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("load repository snapshot %q: %w", targetName, err)
	}

	var stored snapshot
	if err := json.Unmarshal(payload, &stored); err != nil {
		return nil, fmt.Errorf("decode repository snapshot %q: %w", targetName, err)
	}
	if stored.Version != snapshotVersion || stored.RepositoryID != repositoryID {
		return nil, fmt.Errorf("decode repository snapshot %q: metadata mismatch", targetName)
	}
	if err := validateChunks(stored.Chunks); err != nil {
		return nil, fmt.Errorf("decode repository snapshot %q: %w", targetName, err)
	}
	return stored.Chunks, nil
}

func validateRepositoryID(repositoryID string) error {
	if strings.TrimSpace(repositoryID) == "" {
		return ErrInvalidRepositoryID
	}
	return nil
}

func validateChunks(chunks []source.Chunk) error {
	seen := make(map[string]struct{}, len(chunks))
	for index, chunk := range chunks {
		if chunk.ID == "" || chunk.Text == "" || !chunk.Reference.Valid() {
			return fmt.Errorf("%w: chunk %d has incomplete content or provenance", ErrInvalidChunk, index)
		}
		if _, duplicate := seen[chunk.ID]; duplicate {
			return fmt.Errorf("%w: chunk %d duplicates an earlier ID", ErrInvalidChunk, index)
		}
		seen[chunk.ID] = struct{}{}
	}
	return nil
}

func snapshotName(repositoryID string) string {
	digest := sha256.Sum256([]byte(repositoryID))
	return hex.EncodeToString(digest[:]) + ".json"
}

func temporarySnapshotName(targetName string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return "." + targetName + "." + hex.EncodeToString(suffix[:]) + ".tmp", nil
}

func readSnapshot(reader io.Reader) ([]byte, bool, error) {
	payload, err := io.ReadAll(io.LimitReader(reader, maxSnapshotSize+1))
	if err != nil {
		return nil, false, err
	}
	if len(payload) > maxSnapshotSize {
		return nil, true, nil
	}
	return payload, false, nil
}

func syncDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

var _ ingest.Store = (*Store)(nil)
