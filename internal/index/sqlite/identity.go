package sqlite

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ToledoVitor/GoContext/internal/index"
)

const (
	storeIdentityVersion   = 1
	storeIdentitySidecar   = "index-v2.identity.json"
	storeIdentityPrefix    = "gocontext:store-identity:v1:"
	maxIdentitySidecarSize = 512
)

var errStoreIdentity = errors.New("sqlite store identity is invalid")

type storeIdentityDocument struct {
	Version int    `json:"version"`
	StoreID string `json:"store_id"`
}

func invalidStoreIdentity() error {
	return errors.Join(index.ErrReindexRequired, errStoreIdentity)
}

func newStoreIdentity() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", invalidStoreIdentity()
	}
	return hex.EncodeToString(raw[:]), nil
}

func validStoreIdentity(identity string) bool {
	if len(identity) != 64 || identity != strings.ToLower(identity) {
		return false
	}
	decoded, err := hex.DecodeString(identity)
	return err == nil && len(decoded) == 32
}

func storeIdentityRepositoryID(identity string) string {
	return storeIdentityPrefix + identity
}

func isStoreIdentityRepositoryID(repositoryID string) bool {
	return strings.HasPrefix(repositoryID, storeIdentityPrefix)
}

func initializeStoreIdentity(directory, databasePath string) (string, error) {
	sidecarIdentity, sidecarErr := readStoreIdentitySidecar(directory)
	if sidecarErr != nil && !errors.Is(sidecarErr, fs.ErrNotExist) {
		return "", invalidStoreIdentity()
	}

	db, err := sql.Open("sqlite", bootstrapDataSourceName(databasePath))
	if err != nil {
		return "", invalidStoreIdentity()
	}
	closeDatabase := func(operationErr error) (string, error) {
		if closeErr := db.Close(); closeErr != nil && operationErr == nil {
			operationErr = invalidStoreIdentity()
		}
		return "", operationErr
	}
	if err := validateSchema(context.Background(), db); err != nil {
		return closeDatabase(invalidStoreIdentity())
	}
	databaseIdentities, err := loadDatabaseIdentities(context.Background(), db)
	if err != nil || len(databaseIdentities) > 1 {
		return closeDatabase(invalidStoreIdentity())
	}

	identity := sidecarIdentity
	if len(databaseIdentities) == 1 {
		if identity != "" && identity != databaseIdentities[0] {
			return closeDatabase(invalidStoreIdentity())
		}
		identity = databaseIdentities[0]
	}
	if identity == "" {
		identity, err = newStoreIdentity()
		if err != nil {
			return closeDatabase(err)
		}
	}
	if len(databaseIdentities) == 0 {
		if _, err := db.ExecContext(context.Background(),
			`INSERT INTO repositories(repository_id, active_generation) VALUES (?, NULL)`,
			storeIdentityRepositoryID(identity),
		); err != nil {
			return closeDatabase(invalidStoreIdentity())
		}
	}
	if _, err := db.ExecContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return closeDatabase(invalidStoreIdentity())
	}
	if _, err := closeDatabase(nil); err != nil {
		return "", err
	}
	if sidecarIdentity == "" {
		if err := createStoreIdentitySidecar(directory, identity); err != nil {
			return "", invalidStoreIdentity()
		}
	}
	return identity, nil
}

func loadDatabaseIdentities(ctx context.Context, queryer schemaQuerier) ([]string, error) {
	rows, err := queryer.QueryContext(ctx,
		`SELECT repository_id FROM repositories WHERE repository_id LIKE ? ORDER BY repository_id`,
		storeIdentityPrefix+"%",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var identities []string
	for rows.Next() {
		var repositoryID string
		if err := rows.Scan(&repositoryID); err != nil {
			return nil, err
		}
		identity := strings.TrimPrefix(repositoryID, storeIdentityPrefix)
		if repositoryID == storeIdentityRepositoryID(identity) && validStoreIdentity(identity) {
			identities = append(identities, identity)
		} else {
			return nil, invalidStoreIdentity()
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return identities, nil
}

func verifyStoreIdentity(ctx context.Context, queryer schemaQuerier, expected string) error {
	identities, err := loadDatabaseIdentities(ctx, queryer)
	if err == nil && len(identities) == 1 && identities[0] == expected {
		return nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	return invalidStoreIdentity()
}

func readStoreIdentitySidecar(directory string) (string, error) {
	path := filepath.Join(directory, storeIdentitySidecar)
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if err := validatePrivateRegularFile(before); err != nil {
		return "", invalidStoreIdentity()
	}
	file, err := os.Open(path)
	if err != nil {
		return "", invalidStoreIdentity()
	}
	after, statErr := file.Stat()
	if statErr != nil || !os.SameFile(before, after) {
		_ = file.Close()
		return "", invalidStoreIdentity()
	}
	payload, readErr := io.ReadAll(io.LimitReader(file, maxIdentitySidecarSize+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(payload) > maxIdentitySidecarSize {
		return "", invalidStoreIdentity()
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var document storeIdentityDocument
	if err := decoder.Decode(&document); err != nil {
		return "", invalidStoreIdentity()
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", invalidStoreIdentity()
	}
	if document.Version != storeIdentityVersion || !validStoreIdentity(document.StoreID) {
		return "", invalidStoreIdentity()
	}
	return document.StoreID, nil
}

func validatePrivateRegularFile(info fs.FileInfo) error {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return invalidStoreIdentity()
	}
	if err := validatePrivateMode(info); err != nil {
		return invalidStoreIdentity()
	}
	return nil
}

func createStoreIdentitySidecar(directory, identity string) error {
	payload, err := json.Marshal(storeIdentityDocument{Version: storeIdentityVersion, StoreID: identity})
	if err != nil {
		return invalidStoreIdentity()
	}
	temporary, err := os.CreateTemp(directory, ".index-v2.identity.*.tmp")
	if err != nil {
		return invalidStoreIdentity()
	}
	temporaryPath := temporary.Name()
	keepTemporary := true
	defer func() {
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return invalidStoreIdentity()
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return invalidStoreIdentity()
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return invalidStoreIdentity()
	}
	if err := temporary.Close(); err != nil {
		return invalidStoreIdentity()
	}
	target := filepath.Join(directory, storeIdentitySidecar)
	if err := os.Link(temporaryPath, target); err != nil {
		if errors.Is(err, fs.ErrExist) {
			existing, readErr := readStoreIdentitySidecar(directory)
			if readErr == nil && existing == identity {
				return nil
			}
		}
		return invalidStoreIdentity()
	}
	if err := os.Remove(temporaryPath); err != nil {
		return invalidStoreIdentity()
	}
	keepTemporary = false
	return nil
}

func formatStoreIdentityError(operation string) error {
	return fmt.Errorf("%s: %w", operation, invalidStoreIdentity())
}
