package main

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

	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
	"github.com/ToledoVitor/GoContext/internal/source"
)

const (
	rollbackMarkerSchemaVersion = 1
	maxRollbackMarkerSize       = 1024
)

var errInvalidRollbackMarker = errors.New("invalid rollback marker")

type rollbackMarker struct {
	Version          int    `json:"version"`
	RepositoryHash   string `json:"repository_hash"`
	ScanPolicy       string `json:"scan_policy_version"`
	CorpusRevision   string `json:"corpus_revision"`
	ActiveGeneration string `json:"active_generation"`
}

type activeGenerationReader interface {
	ActiveGeneration(context.Context, string) (string, error)
}

func writeRollbackCompanion(
	ctx context.Context,
	storeDirectory string,
	ingested repositoryIngest,
	generationID string,
	active activeGenerationReader,
) error {
	if err := removeRollbackMarker(storeDirectory, ingested.repositoryID); err != nil {
		return err
	}
	snapshotStore, err := localstore.NewStore(storeDirectory)
	if err != nil {
		return err
	}
	if err := snapshotStore.Replace(ctx, ingested.repositoryID, ingested.corpus); err != nil {
		return err
	}
	loaded, err := snapshotStore.Load(ctx, ingested.repositoryID)
	if err != nil {
		return err
	}
	validated, err := source.NewCorpusContext(ctx, ingested.corpus.PolicyVersion, loaded)
	if err != nil || validated.Revision != ingested.corpus.Revision {
		return errors.New("rollback snapshot revision mismatch")
	}
	activeGeneration, err := active.ActiveGeneration(ctx, ingested.repositoryID)
	if err != nil || activeGeneration != generationID {
		return errors.New("rollback active generation mismatch")
	}
	return writeRollbackMarker(ctx, storeDirectory, rollbackMarker{
		Version:          rollbackMarkerSchemaVersion,
		RepositoryHash:   repositoryHash(ingested.repositoryID),
		ScanPolicy:       ingested.corpus.PolicyVersion,
		CorpusRevision:   ingested.corpus.Revision,
		ActiveGeneration: generationID,
	})
}

func rollbackMarkerPath(storeDirectory, repositoryID string) string {
	return filepath.Join(storeDirectory, repositoryHash(repositoryID)+".rollback-ready.json")
}

func readRollbackMarker(ctx context.Context, storeDirectory, repositoryID string) (rollbackMarker, error) {
	if err := ctx.Err(); err != nil {
		return rollbackMarker{}, errInvalidRollbackMarker
	}
	canonicalDirectory, err := canonicalMarkerDirectory(storeDirectory)
	if err != nil {
		return rollbackMarker{}, errInvalidRollbackMarker
	}
	path := filepath.Join(canonicalDirectory, repositoryHash(repositoryID)+".rollback-ready.json")
	before, err := os.Lstat(path)
	if err != nil || validatePrivateRollbackMarker(before) != nil {
		return rollbackMarker{}, errInvalidRollbackMarker
	}
	file, err := openRollbackMarkerNoFollow(path)
	if err != nil {
		return rollbackMarker{}, errInvalidRollbackMarker
	}
	opened, statErr := file.Stat()
	if statErr != nil || validatePrivateRollbackMarker(opened) != nil || !os.SameFile(before, opened) {
		_ = file.Close()
		return rollbackMarker{}, errInvalidRollbackMarker
	}
	payload, readErr := readRollbackMarkerPayload(ctx, file)
	after, pathErr := os.Lstat(path)
	closeErr := file.Close()
	if readErr != nil || pathErr != nil || closeErr != nil ||
		validatePrivateRollbackMarker(after) != nil || !os.SameFile(before, after) || !os.SameFile(opened, after) {
		return rollbackMarker{}, errInvalidRollbackMarker
	}
	if err := ctx.Err(); err != nil {
		return rollbackMarker{}, errInvalidRollbackMarker
	}
	if !hasExactRollbackMarkerFields(payload) {
		return rollbackMarker{}, errInvalidRollbackMarker
	}

	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var marker rollbackMarker
	if err := decoder.Decode(&marker); err != nil {
		return rollbackMarker{}, errInvalidRollbackMarker
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return rollbackMarker{}, errInvalidRollbackMarker
	}
	if marker.Version != rollbackMarkerSchemaVersion ||
		!validSHA256Hex(marker.RepositoryHash) ||
		strings.TrimSpace(marker.ScanPolicy) == "" ||
		!validSHA256Hex(marker.CorpusRevision) ||
		!validSHA256Hex(marker.ActiveGeneration) {
		return rollbackMarker{}, errInvalidRollbackMarker
	}
	return marker, nil
}

func hasExactRollbackMarkerFields(payload []byte) bool {
	expected := map[string]struct{}{
		"version":             {},
		"repository_hash":     {},
		"scan_policy_version": {},
		"corpus_revision":     {},
		"active_generation":   {},
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := make(map[string]struct{}, len(expected))
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, isString := keyToken.(string)
		if err != nil || !isString {
			return false
		}
		if _, allowed := expected[key]; !allowed {
			return false
		}
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return false
		}
	}
	token, err = decoder.Token()
	return err == nil && token == json.Delim('}') && len(seen) == len(expected)
}

func readRollbackMarkerPayload(ctx context.Context, reader io.Reader) ([]byte, error) {
	payload := make([]byte, 0, maxRollbackMarkerSize)
	buffer := make([]byte, 256)
	for len(payload) <= maxRollbackMarkerSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		read, err := reader.Read(buffer)
		payload = append(payload, buffer[:read]...)
		if len(payload) > maxRollbackMarkerSize {
			return nil, errInvalidRollbackMarker
		}
		if errors.Is(err, io.EOF) {
			return payload, nil
		}
		if err != nil {
			return nil, err
		}
		if read == 0 {
			return nil, io.ErrNoProgress
		}
	}
	return nil, errInvalidRollbackMarker
}

func validatePrivateRollbackMarker(info fs.FileInfo) error {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errInvalidRollbackMarker
	}
	return nil
}

func validSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func repositoryHash(repositoryID string) string {
	digest := sha256.Sum256([]byte(repositoryID))
	return hex.EncodeToString(digest[:])
}

func removeRollbackMarker(storeDirectory, repositoryID string) error {
	path := rollbackMarkerPath(storeDirectory, repositoryID)
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncMarkerDirectory(filepath.Dir(path))
}

func writeRollbackMarker(ctx context.Context, storeDirectory string, marker rollbackMarker) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	canonicalDirectory, err := canonicalMarkerDirectory(storeDirectory)
	if err != nil {
		return err
	}
	target := filepath.Join(canonicalDirectory, marker.RepositoryHash+".rollback-ready.json")
	temporary, err := temporaryMarkerPath(canonicalDirectory, marker.RepositoryHash)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	keepTemporary := true
	defer func() {
		if keepTemporary {
			_ = os.Remove(temporary)
		}
	}()
	_, writeErr := io.Copy(file, bytes.NewReader(payload))
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := replaceRollbackMarker(temporary, target, canonicalDirectory); err != nil {
		return err
	}
	keepTemporary = false
	return nil
}

func canonicalMarkerDirectory(directory string) (string, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func temporaryMarkerPath(directory, repositoryHash string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return filepath.Join(directory, fmt.Sprintf(".%s.%s.tmp", repositoryHash, hex.EncodeToString(suffix[:]))), nil
}
