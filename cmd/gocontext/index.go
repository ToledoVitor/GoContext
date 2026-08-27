package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ToledoVitor/GoContext/internal/ingest/filesystem"
	"github.com/ToledoVitor/GoContext/internal/ingest/lineparser"
	"github.com/ToledoVitor/GoContext/internal/ingest/localstore"
	"github.com/ToledoVitor/GoContext/internal/ingest/symbolchunker"
	"github.com/ToledoVitor/GoContext/internal/source"
)

type indexStats struct {
	files   int
	symbols int
	chunks  int
}

func runIndex(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("index", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "uso: gocontext index [--store DIR] <repositório>")
	}
	storeFlag := flags.String("store", "", "diretório local de snapshots")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return 2
	}

	storeDirectory, err := storeDirectory(*storeFlag)
	if err != nil {
		fmt.Fprintf(stderr, "indexar repositório: %v\n", err)
		return 1
	}
	stats, err := indexRepository(ctx, flags.Arg(0), storeDirectory)
	if err != nil {
		fmt.Fprintf(stderr, "indexar repositório: %v\n", err)
		return 1
	}

	fmt.Fprintf(
		stdout,
		"indexado: %d arquivos, %d símbolos, %d chunks\n",
		stats.files,
		stats.symbols,
		stats.chunks,
	)
	return 0
}

func indexRepository(ctx context.Context, repositoryPath, storeDirectory string) (indexStats, error) {
	repositoryID, err := canonicalRepositoryPath(repositoryPath)
	if err != nil {
		return indexStats{}, err
	}

	files, err := filesystem.NewScanner().Scan(ctx, repositoryID)
	if err != nil {
		return indexStats{}, err
	}

	parser := lineparser.NewParser()
	chunker := symbolchunker.NewChunker()
	chunks := make([]source.Chunk, 0)
	stats := indexStats{files: len(files)}
	for _, file := range files {
		symbols, err := parser.Parse(ctx, file)
		if err != nil {
			return indexStats{}, fmt.Errorf("parse %q: %w", file.Reference.Path, err)
		}
		fileChunks, err := chunker.Chunk(ctx, file, symbols)
		if err != nil {
			return indexStats{}, err
		}
		stats.symbols += len(symbols)
		stats.chunks += len(fileChunks)
		chunks = append(chunks, fileChunks...)
	}

	store, err := localstore.NewStore(storeDirectory)
	if err != nil {
		return indexStats{}, err
	}
	if err := store.Replace(ctx, repositoryID, chunks); err != nil {
		return indexStats{}, err
	}
	return stats, nil
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
