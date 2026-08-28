package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	indexdomain "github.com/ToledoVitor/GoContext/internal/index"
	indexsqlite "github.com/ToledoVitor/GoContext/internal/index/sqlite"
	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
)

type indexStats struct {
	files   int
	symbols int
	chunks  int
}

var (
	errSQLiteIndexFailure           = errors.New("falha na indexação SQLite")
	errSQLiteIndexMaintenance       = errors.New("índice SQLite publicado; manutenção incompleta")
	errSQLiteIndexClose             = errors.New("falha ao fechar índice SQLite")
	errRollbackCompanionNotReady    = errors.New("índice SQLite publicado; rollback não está pronto")
	errSnapshotRollbackInvalidation = errors.New("falha ao invalidar prontidão de rollback")
)

func runIndex(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("index", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "uso: gocontext index [--store DIR] [opções semânticas] <repositório>")
		fmt.Fprintln(stderr, "semântica: --semantic off|preferred|required --embedding-base-url URL --embedding-model MODELO --embedding-dimensions N --index-backend snapshot|sqlite")
	}
	storeFlag := flags.String("store", "", "diretório local de snapshots")
	var embeddingFlags embeddingOptions
	backend := indexBackendSnapshot
	addEmbeddingFlags(flags, &embeddingFlags, &backend)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return 2
	}
	resolvedEmbedding, err := resolveCLIEmbeddingConfig(embeddingFlags, backend, commandRoleIndex)
	if err != nil {
		fmt.Fprintf(stderr, "configurar indexação: %v\n", err)
		return 2
	}
	storeDirectory, err := storeDirectory(*storeFlag)
	if err != nil {
		fmt.Fprintf(stderr, "indexar repositório: %v\n", err)
		return 1
	}
	ingested, err := ingestRepository(ctx, flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "indexar repositório: %v\n", err)
		return 1
	}

	var report indexdomain.Report
	var published bool
	switch resolvedEmbedding.backend {
	case indexBackendSnapshot:
		err = publishSnapshot(ctx, storeDirectory, ingested)
		published = err == nil
	case indexBackendSQLite:
		report, published, err = publishSQLiteIndex(ctx, storeDirectory, ingested, resolvedEmbedding, stderr)
	default:
		err = errSQLiteIndexFailure
	}
	if published {
		printIndexReport(stdout, ingested.stats, resolvedEmbedding.mode, report)
	}
	if err != nil {
		fmt.Fprintf(stderr, "indexar repositório: %v\n", err)
		return 1
	}
	return 0
}

func printIndexReport(stdout io.Writer, stats indexStats, mode semanticMode, report indexdomain.Report) {
	fmt.Fprintf(
		stdout,
		"indexado: %d arquivos, %d símbolos, %d chunks\n",
		stats.files, stats.symbols, stats.chunks,
	)
	if mode != semanticModeOff {
		fmt.Fprintf(stdout, "semântica: status=%s vetores=%d requests=%d tokens=%d\n",
			report.Semantic, report.Vectors, report.Requests, report.UsageTokens)
	}
}

func publishSnapshot(ctx context.Context, storeDirectory string, ingested repositoryIngest) error {
	return publishSnapshotWithOperations(ctx, storeDirectory, ingested, snapshotPublicationOperations{})
}

type snapshotPublicationOperations struct {
	removeRollbackMarker func(string, string) error
}

func publishSnapshotWithOperations(
	ctx context.Context,
	storeDirectory string,
	ingested repositoryIngest,
	operations snapshotPublicationOperations,
) error {
	store, err := localstore.NewStore(storeDirectory)
	if err != nil {
		return err
	}
	removeMarker := operations.removeRollbackMarker
	if removeMarker == nil {
		removeMarker = removeRollbackMarker
	}
	if err := removeMarker(storeDirectory, ingested.repositoryID); err != nil {
		return errSnapshotRollbackInvalidation
	}
	if err := store.Replace(ctx, ingested.repositoryID, ingested.corpus); err != nil {
		return err
	}
	return nil
}

func publishSQLiteIndex(
	ctx context.Context,
	storeDirectory string,
	ingested repositoryIngest,
	config resolvedEmbeddingConfig,
	stderr io.Writer,
) (indexdomain.Report, bool, error) {
	store, err := indexsqlite.NewStore(storeDirectory)
	if err != nil {
		return indexdomain.Report{}, false, errSQLiteIndexFailure
	}
	builder, err := indexdomain.NewBuilder(store, config.client, indexdomain.BuilderConfig{Mode: indexSemanticMode(config.mode)})
	if err != nil {
		var operationErr error = errSQLiteIndexFailure
		if closeErr := store.Close(); closeErr != nil {
			operationErr = errors.Join(operationErr, errSQLiteIndexClose)
		}
		return indexdomain.Report{}, false, operationErr
	}
	if config.mode != semanticModeOff && config.egress == dataEgressExternal {
		_, _ = fmt.Fprint(stderr, externalIndexEgressWarning)
	}
	report, buildErr := builder.Replace(ctx, ingested.repositoryID, ingested.corpus)
	var committed *indexdomain.CommittedCleanupError
	published := buildErr == nil || (report.GenerationID != "" && errors.As(buildErr, &committed))
	var operationErr error
	if !published {
		operationErr = errSQLiteIndexFailure
	} else {
		if report.Semantic == indexdomain.SemanticStatusDegraded {
			_, _ = fmt.Fprint(stderr, semanticDegradedWarning)
		}
		if buildErr != nil {
			operationErr = errors.Join(operationErr, errSQLiteIndexMaintenance)
		}
		if err := writeRollbackCompanion(ctx, storeDirectory, ingested, report.GenerationID, store); err != nil {
			operationErr = errors.Join(operationErr, errRollbackCompanionNotReady)
		}
	}
	if closeErr := store.Close(); closeErr != nil {
		operationErr = errors.Join(operationErr, errSQLiteIndexClose)
	}
	return report, published, operationErr
}

func indexSemanticMode(mode semanticMode) indexdomain.SemanticMode {
	switch mode {
	case semanticModePreferred:
		return indexdomain.SemanticPreferred
	case semanticModeRequired:
		return indexdomain.SemanticRequired
	default:
		return indexdomain.SemanticOff
	}
}

func canonicalRepositoryPath(repositoryPath string) (string, error) {
	absolutePath, err := filepath.Abs(repositoryPath)
	if err != nil {
		return "", fmt.Errorf("resolver caminho do repositório: %w", err)
	}
	canonicalPath, err := filepath.EvalSymlinks(absolutePath)
	if err != nil {
		return "", fmt.Errorf("resolver caminho do repositório: %w", err)
	}
	info, err := os.Stat(canonicalPath)
	if err != nil {
		return "", fmt.Errorf("inspecionar repositório: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("inspecionar repositório: caminho não é diretório")
	}
	return canonicalPath, nil
}

func storeDirectory(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	cacheDirectory, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolver cache local: %w", err)
	}
	return filepath.Join(cacheDirectory, "gocontext", "snapshots"), nil
}
