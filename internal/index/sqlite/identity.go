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

func initializeFreshDatabaseIdentity(
	ctx context.Context,
	connection *sql.Conn,
	identity string,
	beforeIdentityInsert func() error,
) (returnedErr error) {
	if !validStoreIdentity(identity) {
		return invalidStoreIdentity()
	}
	if _, err := connection.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return invalidStoreIdentity()
	}
	transactionOpen := true
	defer func() {
		if transactionOpen {
			if _, rollbackErr := connection.ExecContext(context.Background(), `ROLLBACK`); rollbackErr != nil {
				returnedErr = errors.Join(returnedErr, errOpenWriterCleanup)
			}
		}
	}()

	var applicationObjects int
	if err := connection.QueryRowContext(ctx, `
		SELECT count(*)
		FROM sqlite_schema
		WHERE name NOT LIKE 'sqlite_%'`,
	).Scan(&applicationObjects); err != nil || applicationObjects != 0 {
		return invalidStoreIdentity()
	}
	if _, err := connection.ExecContext(ctx, schemaSQL); err != nil {
		return invalidStoreIdentity()
	}
	identities, err := loadDatabaseIdentities(ctx, connection)
	if err != nil || len(identities) != 0 {
		return invalidStoreIdentity()
	}
	if beforeIdentityInsert != nil {
		if err := beforeIdentityInsert(); err != nil {
			return invalidStoreIdentity()
		}
	}
	if _, err := connection.ExecContext(ctx,
		`INSERT INTO repositories(repository_id, active_generation) VALUES (?, NULL)`,
		storeIdentityRepositoryID(identity),
	); err != nil {
		return invalidStoreIdentity()
	}
	if err := validateSchema(ctx, connection); err != nil {
		return invalidStoreIdentity()
	}
	if err := verifyStoreIdentity(ctx, connection, identity); err != nil {
		return invalidStoreIdentity()
	}
	if _, err := connection.ExecContext(ctx, `COMMIT`); err != nil {
		return invalidStoreIdentity()
	}
	transactionOpen = false
	return nil
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

func createStoreIdentitySidecar(
	directory, identity string,
	beforePublish func(string) error,
	afterTargetVisible func(string) error,
	operations storeFileOperations,
) (storePublicationResult, error) {
	payload, err := json.Marshal(storeIdentityDocument{Version: storeIdentityVersion, StoreID: identity})
	if err != nil {
		return storePublicationResult{}, invalidStoreIdentity()
	}
	temporary, err := os.CreateTemp(directory, ".index-v2.identity.*.tmp")
	if err != nil {
		return storePublicationResult{}, invalidStoreIdentity()
	}
	temporaryPath := temporary.Name()
	temporaryInfo, statErr := temporary.Stat()
	fail := func(operationErr error, cleanupErrs ...error) (storePublicationResult, error) {
		cleanupErrs = append(cleanupErrs, removeOwnedTemporaryStoreFile(temporaryPath, directory, temporaryInfo, operations))
		return storePublicationResult{
			cleanupErr:    errors.Join(cleanupErrs...),
			temporaryInfo: temporaryInfo,
			temporaryPath: temporaryPath,
		}, operationErr
	}
	if statErr != nil {
		return fail(invalidStoreIdentity(), temporary.Close())
	}
	if err := temporary.Chmod(0o600); err != nil {
		return fail(invalidStoreIdentity(), temporary.Close())
	}
	if _, err := temporary.Write(payload); err != nil {
		return fail(invalidStoreIdentity(), temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		return fail(invalidStoreIdentity(), temporary.Close())
	}
	afterWriteInfo, err := temporary.Stat()
	if err != nil || !os.SameFile(temporaryInfo, afterWriteInfo) {
		return fail(invalidStoreIdentity(), temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return fail(invalidStoreIdentity())
	}
	target := filepath.Join(directory, storeIdentitySidecar)
	if beforePublish != nil {
		if err := beforePublish(target); err != nil {
			return fail(invalidStoreIdentity())
		}
	}
	result, err := publishStoreFileExclusive(
		temporaryPath,
		target,
		directory,
		operations,
		afterTargetVisible,
	)
	result.temporaryInfo = temporaryInfo
	result.temporaryPath = temporaryPath
	if err != nil {
		var temporaryCleanupErr error
		if !result.temporaryRemoved {
			temporaryCleanupErr = removeOwnedTemporaryStoreFile(temporaryPath, directory, temporaryInfo, operations)
		}
		result.cleanupErr = errors.Join(result.cleanupErr, temporaryCleanupErr)
		return result, invalidStoreIdentity()
	}
	return result, nil
}

func formatStoreIdentityError(operation string) error {
	return fmt.Errorf("%s: %w", operation, invalidStoreIdentity())
}
