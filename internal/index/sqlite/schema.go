package sqlite

const schemaVersion = 2

const generationsTableSQL = `CREATE TABLE IF NOT EXISTS generations (
    repository_id TEXT NOT NULL,
    generation_id TEXT NOT NULL,
    corpus_revision TEXT NOT NULL,
	content_digest TEXT NOT NULL,
    scan_policy_version TEXT NOT NULL,
    profile_fingerprint TEXT,
    profile_model TEXT,
    dimensions INTEGER NOT NULL DEFAULT 0,
	metric TEXT NOT NULL CHECK (metric = 'cosine'),
	vector_digest TEXT NOT NULL,
	manifest_digest TEXT NOT NULL,
    PRIMARY KEY (repository_id, generation_id),
    FOREIGN KEY (repository_id) REFERENCES repositories(repository_id) ON DELETE CASCADE
)`

const schemaSQL = `
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER NOT NULL
);
INSERT INTO schema_version(version)
SELECT 2
WHERE NOT EXISTS (SELECT 1 FROM schema_version);

CREATE TABLE IF NOT EXISTS repositories (
    repository_id TEXT PRIMARY KEY,
    active_generation TEXT,
    FOREIGN KEY (repository_id, active_generation)
        REFERENCES generations(repository_id, generation_id)
        DEFERRABLE INITIALLY DEFERRED
);

` + generationsTableSQL + `;

CREATE TABLE IF NOT EXISTS chunks (
    repository_id TEXT NOT NULL,
    generation_id TEXT NOT NULL,
    chunk_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    text TEXT NOT NULL,
    language TEXT NOT NULL,
    symbol_name TEXT NOT NULL,
    path TEXT NOT NULL,
    start_line INTEGER NOT NULL,
    end_line INTEGER NOT NULL,
    PRIMARY KEY (repository_id, generation_id, chunk_id),
    UNIQUE (repository_id, generation_id, ordinal),
    FOREIGN KEY (repository_id, generation_id)
        REFERENCES generations(repository_id, generation_id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS vectors (
    repository_id TEXT NOT NULL,
    generation_id TEXT NOT NULL,
    chunk_id TEXT NOT NULL,
    encoding_version INTEGER NOT NULL,
    dimensions INTEGER NOT NULL,
    values_blob BLOB NOT NULL,
    PRIMARY KEY (repository_id, generation_id, chunk_id),
    FOREIGN KEY (repository_id, generation_id, chunk_id)
        REFERENCES chunks(repository_id, generation_id, chunk_id) ON DELETE CASCADE
);
`
