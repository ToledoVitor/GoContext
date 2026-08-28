// Package sqlite persists atomic repository index generations in SQLite.
package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ToledoVitor/GoContext/internal/index"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/source"
	_ "modernc.org/sqlite"
)

const databaseName = "index-v2.sqlite3"

var (
	errBoundReaderClosed   = errors.New("bound sqlite reader is closed")
	errPublicationFailed   = errors.New("sqlite index publication failed")
	errOpenExistingFailed  = errors.New("open existing sqlite index store failed")
	errOpenExistingCleanup = errors.New("open existing sqlite index store cleanup failed")
)

func openExistingFailure(operationErr error, cleanupErrs ...error) error {
	var categories []error
	switch {
	case errors.Is(operationErr, index.ErrNotFound):
		categories = append(categories, index.ErrNotFound)
	case errors.Is(operationErr, index.ErrReindexRequired):
		categories = append(categories, index.ErrReindexRequired)
	case errors.Is(operationErr, context.Canceled):
		categories = append(categories, context.Canceled)
	case errors.Is(operationErr, context.DeadlineExceeded):
		categories = append(categories, context.DeadlineExceeded)
	case operationErr != nil:
		categories = append(categories, errOpenExistingFailed)
	}
	for _, cleanupErr := range cleanupErrs {
		if cleanupErr != nil {
			categories = append(categories, errOpenExistingCleanup)
			break
		}
	}
	return errors.Join(categories...)
}

// Store keeps complete repository generations in one local SQLite database.
type Store struct {
	db            *sql.DB
	readOnly      bool
	storeIdentity string
	writeToken    chan struct{}

	lifecycleMu sync.Mutex
	readers     map[*BoundReader]struct{}
	closed      bool
	closeOnce   sync.Once
	closeErr    error
}

// BoundReader holds one immutable repository generation in a read transaction.
type BoundReader struct {
	tx                 *sql.Tx
	repositoryID       string
	generationID       string
	corpusRevision     string
	contentDigest      string
	scanPolicyVersion  string
	profileFingerprint string
	profileModel       string
	dimensions         int
	metric             index.VectorMetric
	store              *Store

	mu     sync.Mutex
	closed bool
}

// NewStore creates or opens a private SQLite corpus store.
func NewStore(directory string) (*Store, error) {
	return newStore(directory, storeOpenHooks{})
}

type storeOpenHooks struct {
	beforeCreateDatabase func(string) error
}

func newStore(directory string, hooks storeOpenHooks) (*Store, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, fmt.Errorf("create sqlite index store: directory is empty")
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("create sqlite index store: resolve directory: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create sqlite index store: create directory: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("create sqlite index store: inspect directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("create sqlite index store: path is not a directory")
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("create sqlite index store: resolve directory: %w", err)
	}
	databasePath := filepath.Join(canonical, databaseName)
	databaseInfo, statErr := os.Lstat(databasePath)
	databaseExists := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("create sqlite index store: inspect database: %w", statErr)
	}
	if databaseExists {
		if err := validateDatabaseFile(databaseInfo, false); err != nil {
			return nil, fmt.Errorf("create sqlite index store: %w", err)
		}
		if err := inspectDatabase(databasePath); err != nil {
			return nil, fmt.Errorf("create sqlite index store: %w", err)
		}
		if err := os.Chmod(canonical, 0o700); err != nil {
			return nil, fmt.Errorf("create sqlite index store: secure directory: %w", err)
		}
	}

	created := false
	if !databaseExists {
		if err := os.Chmod(canonical, 0o700); err != nil {
			return nil, fmt.Errorf("create sqlite index store: secure directory: %w", err)
		}
		if hooks.beforeCreateDatabase != nil {
			if err := hooks.beforeCreateDatabase(databasePath); err != nil {
				return nil, fmt.Errorf("create sqlite index store: prepare database creation: %w", err)
			}
		}
		created, err = createPrivateDatabaseFile(databasePath)
		if err != nil {
			return nil, fmt.Errorf("create sqlite index store: create database: %w", err)
		}
		if !created {
			databaseInfo, err = os.Lstat(databasePath)
			if err != nil {
				return nil, fmt.Errorf("create sqlite index store: inspect database collision: %w", err)
			}
			if err := validateDatabaseFile(databaseInfo, true); err != nil {
				return nil, fmt.Errorf("create sqlite index store: %w", err)
			}
			if err := inspectDatabase(databasePath); err != nil {
				return nil, fmt.Errorf("create sqlite index store: %w", err)
			}
			databaseExists = true
		}
	}
	if created {
		if err := initializeNewDatabase(databasePath); err != nil {
			removeCreatedDatabase(databasePath)
			return nil, fmt.Errorf("create sqlite index store: initialize database: %w", err)
		}
	}
	if !databaseExists {
		if err := inspectDatabase(databasePath); err != nil {
			if created {
				removeCreatedDatabase(databasePath)
			}
			return nil, fmt.Errorf("create sqlite index store: %w", err)
		}
	}
	if databaseExists || created {
		if err := os.Chmod(databasePath, 0o600); err != nil {
			if created {
				removeCreatedDatabase(databasePath)
			}
			return nil, fmt.Errorf("create sqlite index store: secure database: %w", err)
		}
	}
	storeIdentity, err := initializeStoreIdentity(canonical, databasePath)
	if err != nil {
		if created {
			removeCreatedDatabase(databasePath)
		}
		return nil, formatStoreIdentityError("create sqlite index store")
	}

	db, err := sql.Open("sqlite", dataSourceName(databasePath))
	if err != nil {
		return nil, fmt.Errorf("create sqlite index store: open database: %w", err)
	}
	store := &Store{
		db:            db,
		storeIdentity: storeIdentity,
		writeToken:    make(chan struct{}, 1),
		readers:       make(map[*BoundReader]struct{}),
	}
	store.writeToken <- struct{}{}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		if created {
			removeCreatedDatabase(databasePath)
		}
		return nil, fmt.Errorf("create sqlite index store: open operational database: %w", err)
	}
	return store, nil
}

// OpenExisting opens a previously initialized SQLite store without creating or
// changing the requested directory or database when either is absent.
func OpenExisting(directory string) (*Store, error) {
	return openExisting(directory, openExistingHooks{})
}

type openExistingHooks struct {
	beforeOperationalConnection func(string) error
	afterOperationalConnection  func(string) error
}

func openExisting(directory string, hooks openExistingHooks) (*Store, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, fmt.Errorf("open existing sqlite index store: directory is empty")
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("open existing sqlite index store: resolve directory: %w", err)
	}
	info, err := os.Stat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("open existing sqlite index store: %w", index.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("open existing sqlite index store: inspect directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("open existing sqlite index store: path is not a directory")
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("open existing sqlite index store: resolve directory: %w", err)
	}
	databasePath := filepath.Join(canonical, databaseName)
	beforeInfo, err := os.Lstat(databasePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("open existing sqlite index store: %w", index.ErrNotFound)
	} else if err != nil {
		return nil, fmt.Errorf("open existing sqlite index store: inspect database: %w", err)
	}
	canonicalDirectoryInfo, err := os.Stat(canonical)
	if err != nil || !canonicalDirectoryInfo.IsDir() || validatePrivateMode(canonicalDirectoryInfo) != nil {
		return nil, errors.New("open existing sqlite index store: unsafe private directory")
	}
	if err := validateDatabaseFile(beforeInfo, true); err != nil {
		return nil, fmt.Errorf("open existing sqlite index store: %w", err)
	}
	storeIdentity, err := readStoreIdentitySidecar(canonical)
	if err != nil {
		return nil, formatStoreIdentityError("open existing sqlite index store")
	}

	db, err := sql.Open("sqlite", readOnlyDataSourceName(databasePath))
	if err != nil {
		return nil, fmt.Errorf("open existing sqlite index store: open database: %w", err)
	}
	if hooks.beforeOperationalConnection != nil {
		if err := hooks.beforeOperationalConnection(databasePath); err != nil {
			_ = db.Close()
			return nil, formatStoreIdentityError("open existing sqlite index store")
		}
	}
	connection, err := db.Conn(context.Background())
	if err != nil {
		closeErr := db.Close()
		return nil, fmt.Errorf("open existing sqlite index store: %w", openExistingFailure(err, closeErr))
	}
	closeFailure := func(openErr error) (*Store, error) {
		connectionCloseErr := connection.Close()
		databaseCloseErr := db.Close()
		return nil, fmt.Errorf("open existing sqlite index store: %w", openExistingFailure(openErr, connectionCloseErr, databaseCloseErr))
	}
	if hooks.afterOperationalConnection != nil {
		if err := hooks.afterOperationalConnection(databasePath); err != nil {
			return closeFailure(invalidStoreIdentity())
		}
	}
	afterOpenInfo, err := secureDatabaseInfo(databasePath)
	if err != nil {
		return closeFailure(err)
	}
	if !os.SameFile(beforeInfo, afterOpenInfo) {
		return closeFailure(errors.New("database identity changed while opening"))
	}
	if err := validateSchema(context.Background(), connection); err != nil {
		return closeFailure(err)
	}
	if err := verifyStoreIdentity(context.Background(), connection, storeIdentity); err != nil {
		return closeFailure(invalidStoreIdentity())
	}
	afterValidationInfo, err := secureDatabaseInfo(databasePath)
	if err != nil {
		return closeFailure(err)
	}
	if !os.SameFile(beforeInfo, afterValidationInfo) {
		return closeFailure(errors.New("database identity changed while validating"))
	}
	if err := connection.Close(); err != nil {
		databaseCloseErr := db.Close()
		return nil, fmt.Errorf("open existing sqlite index store: %w", openExistingFailure(nil, err, databaseCloseErr))
	}
	store := &Store{
		db:            db,
		readOnly:      true,
		storeIdentity: storeIdentity,
		writeToken:    make(chan struct{}, 1),
		readers:       make(map[*BoundReader]struct{}),
	}
	store.writeToken <- struct{}{}
	return store, nil
}

func secureDatabaseInfo(path string) (fs.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := validateDatabaseFile(info, true); err != nil {
		return nil, err
	}
	return info, nil
}

func validateDatabaseFile(info fs.FileInfo, requirePrivate bool) error {
	if info == nil || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("database path is a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return errors.New("database path is not a regular file")
	}
	if requirePrivate {
		return validatePrivateMode(info)
	}
	return nil
}

func validatePrivateMode(info fs.FileInfo) error {
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return errors.New("unsafe private permissions")
	}
	return nil
}

func createPrivateDatabaseFile(path string) (bool, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	return true, nil
}

func dataSourceName(path string) string {
	dsn := &url.URL{Scheme: "file", Path: path}
	query := dsn.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "secure_delete(ON)")
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

func readOnlyDataSourceName(path string) string {
	dsn := &url.URL{Scheme: "file", Path: path}
	query := dsn.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

func bootstrapDataSourceName(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func inspectionDataSourceName(path string) string {
	dsn := &url.URL{Scheme: "file", Path: path}
	query := dsn.Query()
	query.Set("mode", "ro")
	query.Set("immutable", "1")
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

func initializeNewDatabase(path string) error {
	db, err := sql.Open("sqlite", bootstrapDataSourceName(path))
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(context.Background(), schemaSQL); err != nil {
		_ = db.Close()
		return err
	}
	return db.Close()
}

func inspectDatabase(path string) error {
	db, err := sql.Open("sqlite", inspectionDataSourceName(path))
	if err != nil {
		return err
	}
	validationErr := validateSchema(context.Background(), db)
	closeErr := db.Close()
	if validationErr != nil {
		return validationErr
	}
	return closeErr
}

type schemaQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type schemaColumn struct {
	name               string
	typeName           string
	notNull            int
	primaryKeyPosition int
	defaultSQL         string
	hasDefault         bool
}

type schemaForeignKey struct {
	sequence int
	table    string
	from     string
	to       string
	onUpdate string
	onDelete string
}

var requiredSchemaColumns = map[string][]schemaColumn{
	"schema_version": {
		{name: "version", typeName: "INTEGER", notNull: 1},
	},
	"repositories": {
		{name: "repository_id", typeName: "TEXT", primaryKeyPosition: 1},
		{name: "active_generation", typeName: "TEXT"},
	},
	"generations": {
		{name: "repository_id", typeName: "TEXT", notNull: 1, primaryKeyPosition: 1},
		{name: "generation_id", typeName: "TEXT", notNull: 1, primaryKeyPosition: 2},
		{name: "corpus_revision", typeName: "TEXT", notNull: 1},
		{name: "content_digest", typeName: "TEXT", notNull: 1},
		{name: "scan_policy_version", typeName: "TEXT", notNull: 1},
		{name: "profile_fingerprint", typeName: "TEXT"},
		{name: "profile_model", typeName: "TEXT"},
		{name: "dimensions", typeName: "INTEGER", notNull: 1, defaultSQL: "0", hasDefault: true},
		{name: "metric", typeName: "TEXT", notNull: 1},
	},
	"chunks": {
		{name: "repository_id", typeName: "TEXT", notNull: 1, primaryKeyPosition: 1},
		{name: "generation_id", typeName: "TEXT", notNull: 1, primaryKeyPosition: 2},
		{name: "chunk_id", typeName: "TEXT", notNull: 1, primaryKeyPosition: 3},
		{name: "ordinal", typeName: "INTEGER", notNull: 1},
		{name: "text", typeName: "TEXT", notNull: 1},
		{name: "language", typeName: "TEXT", notNull: 1},
		{name: "symbol_name", typeName: "TEXT", notNull: 1},
		{name: "path", typeName: "TEXT", notNull: 1},
		{name: "start_line", typeName: "INTEGER", notNull: 1},
		{name: "end_line", typeName: "INTEGER", notNull: 1},
	},
	"vectors": {
		{name: "repository_id", typeName: "TEXT", notNull: 1, primaryKeyPosition: 1},
		{name: "generation_id", typeName: "TEXT", notNull: 1, primaryKeyPosition: 2},
		{name: "chunk_id", typeName: "TEXT", notNull: 1, primaryKeyPosition: 3},
		{name: "encoding_version", typeName: "INTEGER", notNull: 1},
		{name: "dimensions", typeName: "INTEGER", notNull: 1},
		{name: "values_blob", typeName: "BLOB", notNull: 1},
	},
}

var requiredSchemaForeignKeys = map[string][]schemaForeignKey{
	"repositories": {
		{sequence: 0, table: "generations", from: "repository_id", to: "repository_id", onUpdate: "NO ACTION", onDelete: "NO ACTION"},
		{sequence: 1, table: "generations", from: "active_generation", to: "generation_id", onUpdate: "NO ACTION", onDelete: "NO ACTION"},
	},
	"generations": {
		{sequence: 0, table: "repositories", from: "repository_id", to: "repository_id", onUpdate: "NO ACTION", onDelete: "CASCADE"},
	},
	"chunks": {
		{sequence: 0, table: "generations", from: "repository_id", to: "repository_id", onUpdate: "NO ACTION", onDelete: "CASCADE"},
		{sequence: 1, table: "generations", from: "generation_id", to: "generation_id", onUpdate: "NO ACTION", onDelete: "CASCADE"},
	},
	"vectors": {
		{sequence: 0, table: "chunks", from: "repository_id", to: "repository_id", onUpdate: "NO ACTION", onDelete: "CASCADE"},
		{sequence: 1, table: "chunks", from: "generation_id", to: "generation_id", onUpdate: "NO ACTION", onDelete: "CASCADE"},
		{sequence: 2, table: "chunks", from: "chunk_id", to: "chunk_id", onUpdate: "NO ACTION", onDelete: "CASCADE"},
	},
}

func validateSchema(ctx context.Context, database schemaQuerier) error {
	var markerTables int
	if err := database.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'schema_version'`,
	).Scan(&markerTables); err != nil {
		return fmt.Errorf("inspect schema: %w", err)
	}
	if markerTables != 1 {
		return index.ErrReindexRequired
	}
	var version, rows int
	if err := database.QueryRowContext(ctx, `SELECT min(version), count(*) FROM schema_version`).Scan(&version, &rows); err != nil {
		return fmt.Errorf("%w: unreadable schema marker", index.ErrReindexRequired)
	}
	if rows != 1 || version != schemaVersion {
		return index.ErrReindexRequired
	}
	var schemaTables int
	if err := database.QueryRowContext(ctx, `
		SELECT count(*) FROM sqlite_schema
		WHERE type = 'table'
		  AND name IN ('schema_version', 'repositories', 'generations', 'chunks', 'vectors')`,
	).Scan(&schemaTables); err != nil {
		return fmt.Errorf("inspect schema tables: %w", err)
	}
	if schemaTables != 5 {
		return index.ErrReindexRequired
	}
	if err := validateCanonicalTableSQL(ctx, database, "generations", generationsTableSQL); err != nil {
		return index.ErrReindexRequired
	}
	for table, columns := range requiredSchemaColumns {
		if err := validateTableColumns(ctx, database, table, columns); err != nil {
			return index.ErrReindexRequired
		}
	}
	for table, foreignKeys := range requiredSchemaForeignKeys {
		if err := validateTableForeignKeys(ctx, database, table, foreignKeys); err != nil {
			return index.ErrReindexRequired
		}
	}
	if err := validateUniqueIndex(ctx, database, "chunks", []string{"repository_id", "generation_id", "ordinal"}); err != nil {
		return index.ErrReindexRequired
	}
	return nil
}

func validateTableColumns(ctx context.Context, database schemaQuerier, table string, expected []schemaColumn) error {
	rows, err := database.QueryContext(ctx, `PRAGMA table_info("`+table+`")`)
	if err != nil {
		return err
	}
	defer rows.Close()

	actual := make([]schemaColumn, 0, len(expected))
	for rows.Next() {
		var cid int
		var column schemaColumn
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &column.name, &column.typeName, &column.notNull, &defaultValue, &column.primaryKeyPosition); err != nil {
			return err
		}
		column.typeName = strings.ToUpper(column.typeName)
		column.defaultSQL = defaultValue.String
		column.hasDefault = defaultValue.Valid
		actual = append(actual, column)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return index.ErrReindexRequired
	}
	for position := range expected {
		if actual[position] != expected[position] {
			return index.ErrReindexRequired
		}
	}
	return nil
}

func validateCanonicalTableSQL(ctx context.Context, database schemaQuerier, table, expected string) error {
	var actual string
	if err := database.QueryRowContext(ctx, `
		SELECT sql
		FROM sqlite_schema
		WHERE type = 'table' AND name = ?`, table).Scan(&actual); err != nil {
		return err
	}
	if canonicalSQLiteDDL(actual) != canonicalSQLiteDDL(expected) {
		return index.ErrReindexRequired
	}
	return nil
}

func canonicalSQLiteDDL(value string) string {
	normalized := strings.Join(strings.Fields(strings.TrimSuffix(strings.TrimSpace(value), ";")), " ")
	return strings.Replace(normalized, "CREATE TABLE IF NOT EXISTS ", "CREATE TABLE ", 1)
}

func validateTableForeignKeys(ctx context.Context, database schemaQuerier, table string, expected []schemaForeignKey) error {
	rows, err := database.QueryContext(ctx, `PRAGMA foreign_key_list("`+table+`")`)
	if err != nil {
		return err
	}
	defer rows.Close()

	actual := make([]schemaForeignKey, 0, len(expected))
	for rows.Next() {
		var id int
		var key schemaForeignKey
		var match string
		if err := rows.Scan(&id, &key.sequence, &key.table, &key.from, &key.to, &key.onUpdate, &key.onDelete, &match); err != nil {
			return err
		}
		actual = append(actual, key)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return index.ErrReindexRequired
	}
	for position := range expected {
		if actual[position] != expected[position] {
			return index.ErrReindexRequired
		}
	}
	return nil
}

func validateUniqueIndex(ctx context.Context, database schemaQuerier, table string, expectedColumns []string) error {
	rows, err := database.QueryContext(ctx, `
		SELECT name
		FROM pragma_index_list(?)
		WHERE "unique" = 1 AND partial = 0
		ORDER BY seq`, table)
	if err != nil {
		return err
	}
	defer rows.Close()

	var candidates []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		candidates = append(candidates, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, candidate := range candidates {
		indexRows, err := database.QueryContext(ctx, `
			SELECT name
			FROM pragma_index_info(?)
			ORDER BY seqno`, candidate)
		if err != nil {
			return err
		}
		var columns []string
		for indexRows.Next() {
			var column string
			if err := indexRows.Scan(&column); err != nil {
				_ = indexRows.Close()
				return err
			}
			columns = append(columns, column)
		}
		iterationErr := indexRows.Err()
		closeErr := indexRows.Close()
		if iterationErr != nil {
			return iterationErr
		}
		if closeErr != nil {
			return closeErr
		}
		if equalStrings(columns, expectedColumns) {
			return nil
		}
	}
	return index.ErrReindexRequired
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for position := range left {
		if left[position] != right[position] {
			return false
		}
	}
	return true
}

func removeCreatedDatabase(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}

// Close releases database resources.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.lifecycleMu.Lock()
		s.closed = true
		readers := make([]*BoundReader, 0, len(s.readers))
		for reader := range s.readers {
			readers = append(readers, reader)
		}
		s.lifecycleMu.Unlock()
		for _, reader := range readers {
			if err := reader.Close(); err != nil && s.closeErr == nil {
				s.closeErr = err
			}
		}
		if err := s.db.Close(); err != nil && s.closeErr == nil {
			s.closeErr = err
		}
	})
	return s.closeErr
}

// Replace publishes a complete generation atomically.
func (s *Store) Replace(ctx context.Context, generation index.Generation) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("replace sqlite index: %w", err)
	}
	if strings.TrimSpace(generation.RepositoryID) == "" || isStoreIdentityRepositoryID(generation.RepositoryID) || strings.TrimSpace(generation.ID) == "" {
		return fmt.Errorf("replace sqlite index: %w", index.ErrInvalidGeneration)
	}
	if s.readOnly {
		return errors.New("replace sqlite index: store is read-only")
	}
	if strings.TrimSpace(generation.ScanPolicyVersion) == "" {
		return fmt.Errorf("replace sqlite index: %w", index.ErrInvalidGeneration)
	}
	if generation.ScanPolicyVersion != ingest.ScanPolicyVersion {
		return fmt.Errorf("replace sqlite index: %w", index.ErrReindexRequired)
	}
	preparedVectors, err := prepareGenerationVectors(generation)
	if err != nil {
		return fmt.Errorf("replace sqlite index: %w", index.ErrInvalidGeneration)
	}
	corpus, err := source.NewCorpus(generation.ScanPolicyVersion, generation.Chunks)
	if err != nil || corpus.Revision != generation.CorpusRevision {
		return fmt.Errorf("replace sqlite index: %w", index.ErrInvalidGeneration)
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("replace sqlite index: %w", ctx.Err())
	case <-s.writeToken:
	}
	defer func() { s.writeToken <- struct{}{} }()

	published, err := s.publish(ctx, generation, preparedVectors)
	if err != nil {
		if published {
			var committed *index.CommittedCleanupError
			if !errors.As(err, &committed) {
				err = index.NewCommittedCleanupError(index.CleanupStagePublicationFinalization)
			}
		} else if contextErr := ctx.Err(); contextErr != nil {
			err = contextErr
		} else if !errors.Is(err, index.ErrConcurrentIndex) && !errors.Is(err, index.ErrInvalidGeneration) {
			err = errPublicationFailed
		}
		return fmt.Errorf("replace sqlite index: %w", err)
	}
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCleanup()
	if err := s.purgeInactive(cleanupCtx); err != nil {
		return fmt.Errorf("replace sqlite index: %w", index.NewCommittedCleanupError(index.CleanupStagePurge))
	}
	if err := s.checkpoint(cleanupCtx); err != nil {
		return fmt.Errorf("replace sqlite index: %w", index.NewCommittedCleanupError(index.CleanupStageCheckpoint))
	}
	return nil
}

func (s *Store) publish(ctx context.Context, generation index.Generation, vectors []preparedVector) (bool, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		published, err := s.publishAttempt(ctx, generation, vectors)
		if err == nil || published {
			return published, err
		}
		if !isSQLiteBusy(err) {
			return false, err
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, fmt.Errorf("%w: database remained busy", index.ErrConcurrentIndex)
		}
		pause := 10 * time.Millisecond
		if remaining < pause {
			pause = remaining
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) publishAttempt(ctx context.Context, generation index.Generation, vectors []preparedVector) (published bool, returnedErr error) {
	connection, err := s.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire write connection: %w", err)
	}
	if err := verifyStoreIdentity(ctx, connection, s.storeIdentity); err != nil {
		_ = connection.Close()
		return false, invalidStoreIdentity()
	}
	return publishOnConnection(ctx, connection, generation, vectors)
}

type writeConnection interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	Close() error
}

func publishOnConnection(ctx context.Context, connection writeConnection, generation index.Generation, vectors []preparedVector) (published bool, returnedErr error) {
	committed := false
	defer func() {
		stage, finalizationErr := finalizeWriteConnection(connection)
		if finalizationErr == nil || returnedErr != nil {
			return
		}
		if committed {
			published = true
			returnedErr = index.NewCommittedCleanupError(stage)
			return
		}
		returnedErr = fmt.Errorf("finalize write connection")
	}()
	if _, err := connection.ExecContext(ctx, `PRAGMA busy_timeout=25`); err != nil {
		return false, fmt.Errorf("configure write connection: %w", err)
	}
	if _, err := connection.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	transactionOpen := true
	defer func() {
		if transactionOpen {
			_, _ = connection.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	if _, err := connection.ExecContext(ctx, `INSERT INTO repositories(repository_id) VALUES (?) ON CONFLICT DO NOTHING`, generation.RepositoryID); err != nil {
		return false, fmt.Errorf("create repository manifest: %w", err)
	}
	var activeGeneration string
	if err := connection.QueryRowContext(ctx,
		`SELECT COALESCE(active_generation, '') FROM repositories WHERE repository_id = ?`,
		generation.RepositoryID,
	).Scan(&activeGeneration); err != nil {
		return false, fmt.Errorf("read repository manifest: %w", err)
	}
	if activeGeneration == generation.ID {
		matches, err := generationMetadataMatches(ctx, connection, generation, vectors)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return false, contextErr
			}
			return false, index.ErrInvalidGeneration
		}
		if !matches {
			return false, index.ErrInvalidGeneration
		}
		return false, nil
	}
	if activeGeneration != generation.BaseGeneration {
		return false, index.ErrConcurrentIndex
	}
	profileFingerprint, profileModel := profileMetadata(generation)
	var storedFingerprint, storedModel any
	if profileFingerprint != "" {
		storedFingerprint = profileFingerprint
	}
	if profileModel != "" {
		storedModel = profileModel
	}
	if _, err := connection.ExecContext(ctx, `
		INSERT INTO generations(
			repository_id, generation_id, corpus_revision, content_digest, scan_policy_version,
			profile_fingerprint, profile_model, dimensions, metric
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		generation.RepositoryID, generation.ID, generation.CorpusRevision, canonicalContentDigest(generation.Chunks), generation.ScanPolicyVersion,
		storedFingerprint, storedModel, generation.Dimensions, string(generation.Metric),
	); err != nil {
		return false, fmt.Errorf("insert generation: %w", err)
	}
	for ordinal, chunk := range generation.Chunks {
		if _, err := connection.ExecContext(ctx, `
			INSERT INTO chunks(
				repository_id, generation_id, chunk_id, ordinal, text, language,
				symbol_name, path, start_line, end_line
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			generation.RepositoryID, generation.ID, chunk.ID, ordinal, chunk.Text,
			string(chunk.Language), chunk.SymbolName, chunk.Reference.Path,
			chunk.Reference.StartLine, chunk.Reference.EndLine,
		); err != nil {
			return false, fmt.Errorf("insert chunk: %w", err)
		}
	}
	for _, vector := range vectors {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if _, err := connection.ExecContext(ctx, `
			INSERT INTO vectors(
				repository_id, generation_id, chunk_id, encoding_version, dimensions, values_blob
			) VALUES (?, ?, ?, ?, ?, ?)`,
			generation.RepositoryID, generation.ID, vector.chunkID, vectorEncodingVersion,
			vector.dimensions, vector.valuesBlob,
		); err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return false, contextErr
			}
			return false, fmt.Errorf("insert vector")
		}
	}
	result, err := connection.ExecContext(ctx, `
		UPDATE repositories
		SET active_generation = ?
		WHERE repository_id = ? AND COALESCE(active_generation, '') = ?`,
		generation.ID, generation.RepositoryID, generation.BaseGeneration,
	)
	if err != nil {
		return false, fmt.Errorf("publish manifest: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("verify manifest publication: %w", err)
	}
	if updated != 1 {
		return false, index.ErrConcurrentIndex
	}
	if _, err := connection.ExecContext(ctx, `COMMIT`); err != nil {
		return false, fmt.Errorf("commit transaction: %w", err)
	}
	transactionOpen = false
	committed = true
	return true, nil
}

func finalizeWriteConnection(connection writeConnection) (index.CleanupStage, error) {
	restoreCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := connection.ExecContext(restoreCtx, `PRAGMA busy_timeout=5000`); err != nil {
		_ = connection.Close()
		return index.CleanupStageConnectionRestore, err
	}
	if err := connection.Close(); err != nil {
		return index.CleanupStageConnectionRelease, err
	}
	return "", nil
}

func (s *Store) purgeInactive(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verifyStoreIdentity(ctx, tx, s.storeIdentity); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM generations
		WHERE NOT EXISTS (
			SELECT 1
			FROM repositories
			WHERE repositories.repository_id = generations.repository_id
			  AND repositories.active_generation = generations.generation_id
		)`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) checkpoint(ctx context.Context) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closed {
		return fmt.Errorf("sqlite store is closed")
	}
	if len(s.readers) != 0 {
		return fmt.Errorf("bound readers remain open")
	}
	connection, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	if err := verifyStoreIdentity(ctx, connection, s.storeIdentity); err != nil {
		return err
	}
	var busy, logFrames, checkpointedFrames int
	if err := connection.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(
		&busy, &logFrames, &checkpointedFrames,
	); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("wal checkpoint remained busy")
	}
	return nil
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type generationMetadataQuerier interface {
	queryRower
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func generationMetadataMatches(ctx context.Context, database generationMetadataQuerier, generation index.Generation, vectors []preparedVector) (bool, error) {
	var revision, contentDigest, policy, fingerprint, model, metric string
	var dimensions, chunks, vectorCount int
	err := database.QueryRowContext(ctx, `
		SELECT g.corpus_revision, g.content_digest, g.scan_policy_version,
		       COALESCE(g.profile_fingerprint, ''), COALESCE(g.profile_model, ''),
		       g.dimensions, g.metric, count(c.chunk_id),
		       (SELECT count(*) FROM vectors AS v
		        WHERE v.repository_id = g.repository_id AND v.generation_id = g.generation_id)
		FROM generations AS g
		LEFT JOIN chunks AS c
		  ON c.repository_id = g.repository_id AND c.generation_id = g.generation_id
		WHERE g.repository_id = ? AND g.generation_id = ?
		GROUP BY g.repository_id, g.generation_id`, generation.RepositoryID, generation.ID,
	).Scan(&revision, &contentDigest, &policy, &fingerprint, &model, &dimensions, &metric, &chunks, &vectorCount)
	if err != nil {
		return false, err
	}
	expectedFingerprint, expectedModel := profileMetadata(generation)
	metadataMatches := revision == generation.CorpusRevision &&
		contentDigest == canonicalContentDigest(generation.Chunks) &&
		policy == generation.ScanPolicyVersion &&
		fingerprint == expectedFingerprint &&
		model == expectedModel &&
		dimensions == generation.Dimensions &&
		metric == string(generation.Metric) &&
		chunks == len(generation.Chunks) &&
		vectorCount == len(vectors)
	if !metadataMatches {
		return false, nil
	}
	rows, err := database.QueryContext(ctx, `
		SELECT chunk_id, encoding_version, dimensions, values_blob
		FROM vectors
		WHERE repository_id = ? AND generation_id = ?
		ORDER BY chunk_id`, generation.RepositoryID, generation.ID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	expected := make(map[string]preparedVector, len(vectors))
	for _, vector := range vectors {
		expected[vector.chunkID] = vector
	}
	seen := 0
	for rows.Next() {
		var chunkID string
		var version, storedDimensions int
		var blob []byte
		if err := rows.Scan(&chunkID, &version, &storedDimensions, &blob); err != nil {
			return false, err
		}
		vector, present := expected[chunkID]
		if !present || version != vectorEncodingVersion || storedDimensions != vector.dimensions || !bytes.Equal(blob, vector.valuesBlob) {
			return false, nil
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return seen == len(vectors), nil
}

func canonicalContentDigest(chunks []source.Chunk) string {
	digest, _ := canonicalContentDigestContext(context.Background(), chunks)
	return digest
}

func canonicalContentDigestContext(ctx context.Context, chunks []source.Chunk) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	digest := sha256.New()
	if err := writeCanonicalStringContext(ctx, digest, "sqlite-canonical-content-v1"); err != nil {
		return "", err
	}
	for ordinal, chunk := range chunks {
		if ordinal%vectorContextStride == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		writeCanonicalInteger(digest, int64(ordinal))
		for _, value := range [...]string{
			chunk.ID,
			chunk.Text,
			string(chunk.Language),
			chunk.SymbolName,
			chunk.Reference.Path,
		} {
			if err := writeCanonicalStringContext(ctx, digest, value); err != nil {
				return "", err
			}
		}
		writeCanonicalInteger(digest, int64(chunk.Reference.StartLine))
		writeCanonicalInteger(digest, int64(chunk.Reference.EndLine))
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func writeCanonicalStringContext(ctx context.Context, writer hash.Hash, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	writeCanonicalInteger(writer, int64(len(value)))
	for offset := 0; offset < len(value); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := offset + 64*1024
		if end > len(value) {
			end = len(value)
		}
		_, _ = writer.Write([]byte(value[offset:end]))
		offset = end
	}
	return nil
}

func writeCanonicalInteger(writer hash.Hash, value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = writer.Write(encoded[:])
}

func isSQLiteBusy(err error) bool {
	var coded interface{ Code() int }
	if !errors.As(err, &coded) {
		return false
	}
	primaryCode := coded.Code() & 0xff
	return primaryCode == 5 || primaryCode == 6
}

func profileMetadata(generation index.Generation) (string, string) {
	if generation.Profile == nil {
		return "", ""
	}
	return generation.Profile.Fingerprint, generation.Profile.Model
}

// ActiveGeneration returns the active generation ID for one repository.
func (s *Store) ActiveGeneration(ctx context.Context, repositoryID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("read active sqlite index: %w", err)
	}
	if strings.TrimSpace(repositoryID) == "" || isStoreIdentityRepositoryID(repositoryID) {
		return "", fmt.Errorf("read active sqlite index: %w", index.ErrInvalidGeneration)
	}
	connection, err := s.db.Conn(ctx)
	if err != nil {
		return "", invalidStoreIdentity()
	}
	defer connection.Close()
	if err := verifyStoreIdentity(ctx, connection, s.storeIdentity); err != nil {
		return "", err
	}
	var generationID sql.NullString
	err = connection.QueryRowContext(ctx, `SELECT active_generation FROM repositories WHERE repository_id = ?`, repositoryID).Scan(&generationID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", index.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read active sqlite index: %w", err)
	}
	if !generationID.Valid {
		return "", index.ErrNotFound
	}
	return generationID.String, nil
}

// BindActive pins the active repository generation in a read transaction.
func (s *Store) BindActive(ctx context.Context, repositoryID string) (*BoundReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("bind active sqlite index: %w", err)
	}
	if strings.TrimSpace(repositoryID) == "" || isStoreIdentityRepositoryID(repositoryID) {
		return nil, fmt.Errorf("bind active sqlite index: %w", index.ErrInvalidGeneration)
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("bind active sqlite index: store is closed")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("bind active sqlite index: begin transaction: %w", err)
	}
	if err := verifyStoreIdentity(ctx, tx, s.storeIdentity); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	var generationID, revision, contentDigest, policy, fingerprint, model, metric string
	var dimensions int
	err = tx.QueryRowContext(ctx, `
		SELECT g.generation_id, g.corpus_revision, g.content_digest, g.scan_policy_version,
		       COALESCE(g.profile_fingerprint, ''), COALESCE(g.profile_model, ''),
		       g.dimensions, g.metric
		FROM repositories AS r
		JOIN generations AS g
		  ON g.repository_id = r.repository_id
		 AND g.generation_id = r.active_generation
		WHERE r.repository_id = ?`, repositoryID,
	).Scan(&generationID, &revision, &contentDigest, &policy, &fingerprint, &model, &dimensions, &metric)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return nil, index.ErrNotFound
	}
	if err != nil {
		_ = tx.Rollback()
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, fmt.Errorf("bind active sqlite index: %w", contextErr)
		}
		return nil, index.ErrReindexRequired
	}
	if policy != ingest.ScanPolicyVersion {
		_ = tx.Rollback()
		return nil, index.ErrReindexRequired
	}
	if metric != string(index.VectorMetricCosine) || !validStoredVectorMetadata(fingerprint, model, dimensions) {
		_ = tx.Rollback()
		return nil, index.ErrReindexRequired
	}
	reader := &BoundReader{
		tx:                 tx,
		repositoryID:       repositoryID,
		generationID:       generationID,
		corpusRevision:     revision,
		contentDigest:      contentDigest,
		scanPolicyVersion:  policy,
		profileFingerprint: fingerprint,
		profileModel:       model,
		dimensions:         dimensions,
		metric:             index.VectorMetric(metric),
		store:              s,
	}
	s.readers[reader] = struct{}{}
	return reader, nil
}

func validStoredVectorMetadata(fingerprint, model string, dimensions int) bool {
	if fingerprint == "" && model == "" {
		return dimensions == 0
	}
	return strings.TrimSpace(fingerprint) != "" && strings.TrimSpace(model) != "" && dimensions > 0
}

// GenerationID returns the generation pinned by the reader.
func (r *BoundReader) GenerationID() string {
	if r == nil {
		return ""
	}
	return r.generationID
}

// Load returns canonical chunks from the reader's pinned generation.
func (r *BoundReader) Load(ctx context.Context, repositoryID string) ([]source.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("load bound sqlite index: %w", err)
	}
	if r == nil || repositoryID != r.repositoryID {
		return nil, fmt.Errorf("load bound sqlite index: %w", index.ErrInvalidGeneration)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, fmt.Errorf("load bound sqlite index: %w", errBoundReaderClosed)
	}
	rows, err := r.tx.QueryContext(ctx, `
		SELECT c.chunk_id, c.text, c.language, c.symbol_name, c.path, c.start_line, c.end_line
		FROM chunks AS c
		WHERE c.repository_id = ? AND c.generation_id = ?
		ORDER BY c.ordinal`, r.repositoryID, r.generationID)
	if err != nil {
		return nil, boundLoadError(ctx)
	}

	chunks := make([]source.Chunk, 0)
	for rows.Next() {
		var chunk source.Chunk
		var language string
		if err := rows.Scan(
			&chunk.ID, &chunk.Text, &language, &chunk.SymbolName,
			&chunk.Reference.Path, &chunk.Reference.StartLine, &chunk.Reference.EndLine,
		); err != nil {
			_ = rows.Close()
			return nil, boundLoadError(ctx)
		}
		chunk.Language = source.Language(language)
		chunks = append(chunks, chunk)
	}
	iterationErr := rows.Err()
	closeErr := rows.Close()
	if iterationErr != nil || closeErr != nil {
		return nil, boundLoadError(ctx)
	}
	corpus, err := source.NewCorpus(r.scanPolicyVersion, chunks)
	if err != nil || corpus.Revision != r.corpusRevision || canonicalContentDigest(chunks) != r.contentDigest {
		return nil, index.ErrReindexRequired
	}
	return chunks, nil
}

func boundLoadError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("load bound sqlite index: %w", err)
	}
	return index.ErrReindexRequired
}

// Close releases the bound read transaction. It is safe to call repeatedly.
func (r *BoundReader) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	err := r.tx.Rollback()
	r.mu.Unlock()
	if r.store != nil {
		r.store.unregisterReader(r)
	}
	if errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return err
}

func (s *Store) unregisterReader(reader *BoundReader) {
	s.lifecycleMu.Lock()
	delete(s.readers, reader)
	s.lifecycleMu.Unlock()
}

// Load returns canonical chunks from the active repository generation.
func (s *Store) Load(ctx context.Context, repositoryID string) ([]source.Chunk, error) {
	reader, err := s.BindActive(ctx, repositoryID)
	if err != nil {
		return nil, fmt.Errorf("load sqlite index: %w", err)
	}
	return loadAndClose(ctx, repositoryID, reader)
}

type corpusReadCloser interface {
	Load(context.Context, string) ([]source.Chunk, error)
	Close() error
}

func loadAndClose(ctx context.Context, repositoryID string, reader corpusReadCloser) ([]source.Chunk, error) {
	chunks, err := reader.Load(ctx, repositoryID)
	closeErr := reader.Close()
	if err != nil && closeErr != nil {
		return nil, fmt.Errorf("load sqlite index: %w", errors.Join(err, fmt.Errorf("close bound reader: %w", closeErr)))
	}
	if err != nil {
		return nil, fmt.Errorf("load sqlite index: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("load sqlite index: close bound reader: %w", closeErr)
	}
	return chunks, nil
}

var _ index.Store = (*Store)(nil)
