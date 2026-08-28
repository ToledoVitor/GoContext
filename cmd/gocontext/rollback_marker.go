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
	"os"
	"path/filepath"

	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
	"github.com/ToledoVitor/GoContext/internal/source"
)

const rollbackMarkerSchemaVersion = 1

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
