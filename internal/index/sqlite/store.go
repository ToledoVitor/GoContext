// Package sqlite persists atomic repository index generations in SQLite.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ToledoVitor/GoContext/internal/index"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/source"
	_ "modernc.org/sqlite"
)

const databaseName = "index-v2.sqlite3"

// Store keeps complete repository generations in one local SQLite database.
type Store struct {
	db         *sql.DB
	writeToken chan struct{}
}

// BoundReader holds one immutable repository generation in a read transaction.
type BoundReader struct {
	tx                *sql.Tx
	repositoryID      string
	generationID      string
	corpusRevision    string
	scanPolicyVersion string

	mu     sync.Mutex
	closed bool
}

// NewStore creates or opens a private SQLite corpus store.
func NewStore(directory string) (*Store, error) {
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
	if err := os.Chmod(abs, 0o700); err != nil {
		return nil, fmt.Errorf("create sqlite index store: secure directory: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("create sqlite index store: resolve directory: %w", err)
	}
	databasePath := filepath.Join(canonical, databaseName)
	created, err := createPrivateDatabaseFile(databasePath)
	if err != nil {
		return nil, fmt.Errorf("create sqlite index store: create database: %w", err)
	}

	db, err := sql.Open("sqlite", dataSourceName(databasePath))
	if err != nil {
		return nil, fmt.Errorf("create sqlite index store: open database: %w", err)
	}
	store := &Store{db: db, writeToken: make(chan struct{}, 1)}
	store.writeToken <- struct{}{}
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		if created {
			_ = os.Remove(databasePath)
		}
		return nil, fmt.Errorf("create sqlite index store: %w", err)
	}
	return store, nil
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

func (s *Store) initialize(ctx context.Context) error {
	var markerTables int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'schema_version'`,
	).Scan(&markerTables); err != nil {
		return fmt.Errorf("inspect schema: %w", err)
	}
	if markerTables == 0 {
		var applicationTables int
		if err := s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`,
		).Scan(&applicationTables); err != nil {
			return fmt.Errorf("inspect schema: %w", err)
		}
		if applicationTables != 0 {
			return index.ErrReindexRequired
		}
		if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("initialize schema: %w", err)
		}
	}
	var version, rows int
	if err := s.db.QueryRowContext(ctx, `SELECT min(version), count(*) FROM schema_version`).Scan(&version, &rows); err != nil {
		return fmt.Errorf("%w: unreadable schema marker", index.ErrReindexRequired)
	}
	if rows != 1 || version != schemaVersion {
		return index.ErrReindexRequired
	}
	var schemaTables int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM sqlite_schema
		WHERE type = 'table'
		  AND name IN ('schema_version', 'repositories', 'generations', 'chunks', 'vectors')`,
	).Scan(&schemaTables); err != nil {
		return fmt.Errorf("inspect schema tables: %w", err)
	}
	if schemaTables != 5 {
		return index.ErrReindexRequired
	}
	return nil
}

// Close releases database resources.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Replace publishes a complete generation atomically.
func (s *Store) Replace(ctx context.Context, generation index.Generation) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("replace sqlite index: %w", err)
	}
	if strings.TrimSpace(generation.RepositoryID) == "" || strings.TrimSpace(generation.ID) == "" {
		return fmt.Errorf("replace sqlite index: %w", index.ErrInvalidGeneration)
	}
	if strings.TrimSpace(generation.ScanPolicyVersion) == "" {
		return fmt.Errorf("replace sqlite index: %w", index.ErrInvalidGeneration)
	}
	if generation.ScanPolicyVersion != ingest.ScanPolicyVersion {
		return fmt.Errorf("replace sqlite index: %w", index.ErrReindexRequired)
	}
	if len(generation.Vectors) != 0 {
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

	_, err = s.publish(ctx, generation)
	if err != nil {
		return fmt.Errorf("replace sqlite index: %w", err)
	}
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCleanup()
	if err := s.purgeInactive(cleanupCtx); err != nil {
		return fmt.Errorf("replace sqlite index: purge inactive generations: %w", err)
	}
	if err := s.checkpoint(cleanupCtx); err != nil {
		return fmt.Errorf("replace sqlite index: checkpoint database: %w", err)
	}
	return nil
}

func (s *Store) publish(ctx context.Context, generation index.Generation) (bool, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		published, err := s.publishAttempt(ctx, generation)
		if err == nil {
			return published, nil
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

func (s *Store) publishAttempt(ctx context.Context, generation index.Generation) (published bool, returnedErr error) {
	connection, err := s.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire write connection: %w", err)
	}
	defer func() {
		restoreCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := connection.ExecContext(restoreCtx, `PRAGMA busy_timeout=5000`); returnedErr == nil && err != nil {
			returnedErr = fmt.Errorf("restore write connection: %w", err)
		}
		if err := connection.Close(); returnedErr == nil && err != nil {
			returnedErr = fmt.Errorf("release write connection: %w", err)
		}
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
		matches, err := generationMetadataMatches(ctx, connection, generation)
		if err != nil {
			return false, fmt.Errorf("verify active generation: %w", err)
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
			repository_id, generation_id, corpus_revision, scan_policy_version,
			profile_fingerprint, profile_model, dimensions
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		generation.RepositoryID, generation.ID, generation.CorpusRevision, generation.ScanPolicyVersion,
		storedFingerprint, storedModel, generation.Dimensions,
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
	return true, nil
}

func (s *Store) purgeInactive(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
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
	var busy, logFrames, checkpointedFrames int
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(
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

func generationMetadataMatches(ctx context.Context, database queryRower, generation index.Generation) (bool, error) {
	var revision, policy, fingerprint, model string
	var dimensions, chunks int
	err := database.QueryRowContext(ctx, `
		SELECT g.corpus_revision, g.scan_policy_version,
		       COALESCE(g.profile_fingerprint, ''), COALESCE(g.profile_model, ''),
		       g.dimensions, count(c.chunk_id)
		FROM generations AS g
		LEFT JOIN chunks AS c
		  ON c.repository_id = g.repository_id AND c.generation_id = g.generation_id
		WHERE g.repository_id = ? AND g.generation_id = ?
		GROUP BY g.repository_id, g.generation_id`, generation.RepositoryID, generation.ID,
	).Scan(&revision, &policy, &fingerprint, &model, &dimensions, &chunks)
	if err != nil {
		return false, err
	}
	expectedFingerprint, expectedModel := profileMetadata(generation)
	return revision == generation.CorpusRevision &&
		policy == generation.ScanPolicyVersion &&
		fingerprint == expectedFingerprint &&
		model == expectedModel &&
		dimensions == generation.Dimensions &&
		chunks == len(generation.Chunks), nil
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
	if strings.TrimSpace(repositoryID) == "" {
		return "", fmt.Errorf("read active sqlite index: %w", index.ErrInvalidGeneration)
	}
	var generationID sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT active_generation FROM repositories WHERE repository_id = ?`, repositoryID).Scan(&generationID)
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
	if strings.TrimSpace(repositoryID) == "" {
		return nil, fmt.Errorf("bind active sqlite index: %w", index.ErrInvalidGeneration)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("bind active sqlite index: begin transaction: %w", err)
	}
	var generationID, revision, policy string
	err = tx.QueryRowContext(ctx, `
		SELECT g.generation_id, g.corpus_revision, g.scan_policy_version
		FROM repositories AS r
		JOIN generations AS g
		  ON g.repository_id = r.repository_id
		 AND g.generation_id = r.active_generation
		WHERE r.repository_id = ?`, repositoryID,
	).Scan(&generationID, &revision, &policy)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return nil, index.ErrNotFound
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("bind active sqlite index: read manifest: %w", err)
	}
	if policy != ingest.ScanPolicyVersion {
		_ = tx.Rollback()
		return nil, index.ErrReindexRequired
	}
	return &BoundReader{
		tx:                tx,
		repositoryID:      repositoryID,
		generationID:      generationID,
		corpusRevision:    revision,
		scanPolicyVersion: policy,
	}, nil
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
		return nil, fmt.Errorf("load bound sqlite index: reader is closed")
	}
	rows, err := r.tx.QueryContext(ctx, `
		SELECT c.chunk_id, c.text, c.language, c.symbol_name, c.path, c.start_line, c.end_line
		FROM chunks AS c
		WHERE c.repository_id = ? AND c.generation_id = ?
		ORDER BY c.ordinal`, r.repositoryID, r.generationID)
	if err != nil {
		return nil, fmt.Errorf("load bound sqlite index: query chunks: %w", err)
	}
	defer rows.Close()

	chunks := make([]source.Chunk, 0)
	for rows.Next() {
		var chunk source.Chunk
		var language string
		if err := rows.Scan(
			&chunk.ID, &chunk.Text, &language, &chunk.SymbolName,
			&chunk.Reference.Path, &chunk.Reference.StartLine, &chunk.Reference.EndLine,
		); err != nil {
			return nil, fmt.Errorf("load bound sqlite index: scan chunk: %w", err)
		}
		chunk.Language = source.Language(language)
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load bound sqlite index: read chunks: %w", err)
	}
	corpus, err := source.NewCorpus(r.scanPolicyVersion, chunks)
	if err != nil || corpus.Revision != r.corpusRevision {
		return nil, index.ErrReindexRequired
	}
	return chunks, nil
}

// Close releases the bound read transaction. It is safe to call repeatedly.
func (r *BoundReader) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.tx.Rollback()
	if errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return err
}

// Load returns canonical chunks from the active repository generation.
func (s *Store) Load(ctx context.Context, repositoryID string) ([]source.Chunk, error) {
	reader, err := s.BindActive(ctx, repositoryID)
	if err != nil {
		return nil, fmt.Errorf("load sqlite index: %w", err)
	}
	defer reader.Close()
	chunks, err := reader.Load(ctx, repositoryID)
	if err != nil {
		return nil, fmt.Errorf("load sqlite index: %w", err)
	}
	return chunks, nil
}

var _ index.Store = (*Store)(nil)
