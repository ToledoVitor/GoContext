package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/ToledoVitor/GoContext/internal/index"
	indexsqlite "github.com/ToledoVitor/GoContext/internal/index/sqlite"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
	searchdomain "github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/search/hybrid"
	"github.com/ToledoVitor/GoContext/internal/search/lexical"
	vectorsearch "github.com/ToledoVitor/GoContext/internal/search/vector"
	"github.com/ToledoVitor/GoContext/internal/source"
)

const defaultSearchLimit = 10

var (
	errSQLiteIndexUnavailable        = errors.New("índice SQLite indisponível")
	errSQLiteIndexInvalid            = errors.New("índice SQLite inválido; reindexe o repositório")
	errSQLiteSearchFailure           = errors.New("falha na busca SQLite")
	errSQLiteSearchClose             = errors.New("falha ao fechar busca SQLite")
	errSnapshotRollbackReindex error = snapshotRollbackReindexError{}
)

type snapshotRollbackReindexError struct{}

func (snapshotRollbackReindexError) Error() string {
	return "rollback de snapshot exige reindexação"
}

func (snapshotRollbackReindexError) Unwrap() error {
	return index.ErrReindexRequired
}

func runSearch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("search", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "uso: gocontext search [--store DIR] [--limit N] [--path-prefix PREFIX] [--language python|typescript] [opções semânticas] <repositório> <consulta...>")
		fmt.Fprintln(stderr, "semântica: --semantic off|preferred|required --embedding-base-url URL --embedding-model MODELO --embedding-dimensions N --index-backend snapshot|sqlite|auto")
	}
	storeFlag := flags.String("store", "", "diretório local de snapshots")
	limit := flags.Int("limit", defaultSearchLimit, "máximo de resultados")
	var filterFlags searchFilterOptions
	addSearchFilterFlags(flags, &filterFlags)
	var embeddingFlags embeddingOptions
	backend := indexBackendSnapshot
	addEmbeddingFlags(flags, &embeddingFlags, &backend)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() < 2 || *limit <= 0 {
		flags.Usage()
		return 2
	}
	queryText := strings.TrimSpace(strings.Join(flags.Args()[1:], " "))
	if queryText == "" {
		flags.Usage()
		return 2
	}
	filter, err := resolveSearchFilter(filterFlags)
	if err != nil {
		fmt.Fprintf(stderr, "configurar busca: %v\n", err)
		return 2
	}
	resolvedEmbedding, err := resolveCLIEmbeddingConfig(embeddingFlags, backend, commandRoleSearch)
	if err != nil {
		fmt.Fprintf(stderr, "configurar busca: %v\n", err)
		return 2
	}
	storePath, err := storeDirectory(*storeFlag)
	if err != nil {
		fmt.Fprintf(stderr, "consultar repositório: %v\n", err)
		return 1
	}
	repositoryID, err := canonicalRepositoryPath(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "consultar repositório: %v\n", err)
		return 1
	}
	query := searchdomain.Query{
		RepositoryID: repositoryID,
		Text:         queryText,
		Limit:        *limit,
		Filter:       filter,
	}
	var hits []searchdomain.Hit
	if resolvedEmbedding.backend == indexBackendSnapshot {
		if resolvedEmbedding.backendExplicit {
			hits, err = searchExplicitSnapshotRollback(ctx, storePath, query)
		} else {
			hits, err = searchSnapshot(ctx, storePath, query)
		}
	} else {
		hits, err = searchSQLite(ctx, storePath, query, resolvedEmbedding, stderr)
	}
	if err != nil {
		fmt.Fprintf(stderr, "consultar repositório: %v\n", err)
		return 1
	}
	if len(hits) == 0 {
		fmt.Fprintln(stdout, "nenhum resultado")
		return 0
	}

	for index, hit := range hits {
		if index > 0 {
			fmt.Fprintln(stdout)
		}
		reference := hit.Chunk.Reference
		fmt.Fprintf(
			stdout,
			"%.3f %s:%d-%d",
			hit.Score,
			safeTerminalValue(reference.Path, false),
			reference.StartLine,
			reference.EndLine,
		)
		if hit.Chunk.SymbolName != "" {
			fmt.Fprintf(stdout, " %s", safeTerminalValue(hit.Chunk.SymbolName, false))
		}
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, safeTerminalValue(hit.Chunk.Text, true))
	}
	return 0
}

type fixedSnapshotLoader struct {
	repositoryID string
	chunks       []source.Chunk
}

func (loader fixedSnapshotLoader) Load(ctx context.Context, repositoryID string) ([]source.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if repositoryID != loader.repositoryID {
		return nil, errSnapshotRollbackReindex
	}
	return append([]source.Chunk(nil), loader.chunks...), nil
}

func searchExplicitSnapshotRollback(
	ctx context.Context,
	storePath string,
	query searchdomain.Query,
) ([]searchdomain.Hit, error) {
	store, err := indexsqlite.OpenExistingContext(ctx, storePath)
	if err != nil {
		if errors.Is(err, index.ErrNotFound) {
			return searchExistingSnapshot(ctx, storePath, query)
		}
		return nil, errSnapshotRollbackReindex
	}
	reader, err := store.BindActive(ctx, query.RepositoryID)
	if err != nil {
		closeErr := store.Close()
		if errors.Is(err, index.ErrNotFound) && closeErr == nil {
			return searchExistingSnapshot(ctx, storePath, query)
		}
		return nil, errSnapshotRollbackReindex
	}

	metadata, metadataErr := reader.CorpusMetadata(ctx)
	marker, markerErr := readRollbackMarker(ctx, storePath, query.RepositoryID)
	snapshotStore, snapshotStoreErr := localstore.OpenExisting(storePath)
	var chunks []source.Chunk
	var snapshotErr error
	if snapshotStoreErr == nil {
		chunks, snapshotErr = snapshotStore.Load(ctx, query.RepositoryID)
	}
	var corpus source.Corpus
	if snapshotErr == nil && snapshotStoreErr == nil {
		corpus, snapshotErr = source.NewCorpusContext(ctx, ingest.ScanPolicyVersion, chunks)
	}
	valid := metadataErr == nil && markerErr == nil && snapshotStoreErr == nil && snapshotErr == nil &&
		metadata.ScanPolicyVersion == ingest.ScanPolicyVersion &&
		marker.RepositoryHash == repositoryHash(query.RepositoryID) &&
		marker.ScanPolicy == ingest.ScanPolicyVersion &&
		marker.CorpusRevision == corpus.Revision &&
		marker.CorpusRevision == metadata.CorpusRevision &&
		marker.ActiveGeneration == metadata.GenerationID
	if !valid {
		_ = reader.Close()
		_ = store.Close()
		return nil, errSnapshotRollbackReindex
	}

	searcher, err := lexical.NewSearcher(fixedSnapshotLoader{repositoryID: query.RepositoryID, chunks: chunks})
	if err == nil {
		chunks = nil
		var hits []searchdomain.Hit
		hits, err = searcher.Search(ctx, query)
		if closeErr := closeSQLiteSearch(reader, store, err); closeErr != nil {
			return nil, errSnapshotRollbackReindex
		}
		return hits, nil
	}
	_ = reader.Close()
	_ = store.Close()
	return nil, errSnapshotRollbackReindex
}

func searchSnapshot(ctx context.Context, storePath string, query searchdomain.Query) ([]searchdomain.Hit, error) {
	store, err := localstore.NewStore(storePath)
	if err != nil {
		return nil, err
	}
	searcher, err := lexical.NewSearcher(store)
	if err != nil {
		return nil, err
	}
	return searcher.Search(ctx, query)
}

func searchExistingSnapshot(ctx context.Context, storePath string, query searchdomain.Query) ([]searchdomain.Hit, error) {
	store, err := localstore.OpenExisting(storePath)
	if err != nil {
		return nil, err
	}
	searcher, err := lexical.NewSearcher(store)
	if err != nil {
		return nil, err
	}
	return searcher.Search(ctx, query)
}

func searchSQLite(
	ctx context.Context,
	storePath string,
	query searchdomain.Query,
	config resolvedEmbeddingConfig,
	stderr io.Writer,
) ([]searchdomain.Hit, error) {
	store, err := indexsqlite.OpenExistingContext(ctx, storePath)
	if err != nil {
		if errors.Is(err, index.ErrNotFound) {
			return searchMissingSQLite(ctx, storePath, query, config, stderr)
		}
		return nil, errSQLiteIndexInvalid
	}
	reader, err := store.BindActive(ctx, query.RepositoryID)
	if err != nil {
		closeErr := store.Close()
		if errors.Is(err, index.ErrNotFound) {
			hits, fallbackErr := searchMissingSQLite(ctx, storePath, query, config, stderr)
			if closeErr != nil {
				fallbackErr = errors.Join(fallbackErr, errSQLiteSearchClose)
			}
			return hits, fallbackErr
		}
		if closeErr != nil {
			return nil, errors.Join(errSQLiteIndexInvalid, errSQLiteSearchClose)
		}
		return nil, errSQLiteIndexInvalid
	}
	chunks, err := reader.Load(ctx, query.RepositoryID)
	if err != nil {
		return nil, closeSQLiteSearch(reader, store, errSQLiteIndexInvalid)
	}
	if len(chunks) > exactSearchWarningThreshold {
		_, _ = fmt.Fprint(stderr, exactSearchScaleWarning)
	}

	lexicalSearcher, err := lexical.NewSearcher(fixedSnapshotLoader{repositoryID: query.RepositoryID, chunks: chunks})
	if err != nil {
		return nil, closeSQLiteSearch(reader, store, errSQLiteSearchFailure)
	}
	var searcher searchdomain.Searcher = lexicalSearcher
	if config.mode != semanticModeOff {
		vectorSearcher, vectorErr := vectorsearch.NewSearcher(config.client, reader)
		if vectorErr != nil {
			return nil, closeSQLiteSearch(reader, store, errSQLiteSearchFailure)
		}
		observer := &cliHybridObserver{writer: stderr}
		hybridSearcher, hybridErr := hybrid.NewSearcher(lexicalSearcher, vectorSearcher, observer, hybrid.Config{
			Mode: hybridSemanticMode(config.mode),
		})
		if hybridErr != nil {
			return nil, closeSQLiteSearch(reader, store, errSQLiteSearchFailure)
		}
		searcher = hybridSearcher
		if config.egress == dataEgressExternal {
			_, _ = fmt.Fprint(stderr, externalSearchEgressWarning)
		}
	}

	hits, searchErr := searcher.Search(ctx, query)
	if searchErr != nil {
		searchErr = errSQLiteSearchFailure
	}
	return hits, closeSQLiteSearch(reader, store, searchErr)
}

func closeSQLiteSearch(reader *indexsqlite.BoundReader, store *indexsqlite.Store, operationErr error) error {
	readerCloseErr := reader.Close()
	storeCloseErr := store.Close()
	if readerCloseErr != nil || storeCloseErr != nil {
		return errors.Join(operationErr, errSQLiteSearchClose)
	}
	return operationErr
}

func searchMissingSQLite(
	ctx context.Context,
	storePath string,
	query searchdomain.Query,
	config resolvedEmbeddingConfig,
	stderr io.Writer,
) ([]searchdomain.Hit, error) {
	fallback := (config.backend == indexBackendAuto && config.mode != semanticModeRequired) ||
		(config.backend == indexBackendSQLite && config.mode == semanticModePreferred)
	if !fallback {
		return nil, errSQLiteIndexUnavailable
	}
	if config.mode == semanticModePreferred {
		_, _ = fmt.Fprint(stderr, semanticDegradedWarning)
	}
	return searchExistingSnapshot(ctx, storePath, query)
}

func hybridSemanticMode(mode semanticMode) hybrid.SemanticMode {
	switch mode {
	case semanticModePreferred:
		return hybrid.SemanticPreferred
	case semanticModeRequired:
		return hybrid.SemanticRequired
	default:
		return hybrid.SemanticOff
	}
}

func safeTerminalValue(value string, multiline bool) string {
	var safe strings.Builder
	for _, character := range value {
		if multiline && (character == '\n' || character == '\t') {
			safe.WriteRune(character)
			continue
		}
		if !unicode.IsControl(character) {
			safe.WriteRune(character)
			continue
		}

		switch {
		case character <= 0xff:
			fmt.Fprintf(&safe, `\x%02x`, character)
		case character <= 0xffff:
			fmt.Fprintf(&safe, `\u%04x`, character)
		default:
			fmt.Fprintf(&safe, `\U%08x`, character)
		}
	}
	return safe.String()
}
