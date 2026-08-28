package main

import (
	"context"
	"errors"

	"github.com/ToledoVitor/GoContext/internal/ingest/filesystem"
	"github.com/ToledoVitor/GoContext/internal/ingest/lineparser"
	"github.com/ToledoVitor/GoContext/internal/ingest/symbolchunker"
	"github.com/ToledoVitor/GoContext/internal/source"
)

type repositoryIngest struct {
	repositoryID string
	corpus       source.Corpus
	stats        indexStats
}

func ingestRepository(ctx context.Context, repositoryPath string) (repositoryIngest, error) {
	repositoryID, err := canonicalRepositoryPath(repositoryPath)
	if err != nil {
		return repositoryIngest{}, err
	}
	scanResult, err := filesystem.NewScanner().Scan(ctx, repositoryID)
	if err != nil {
		return repositoryIngest{}, err
	}

	parser := lineparser.NewParser()
	chunker := symbolchunker.NewChunker()
	chunks := make([]source.Chunk, 0)
	stats := indexStats{files: len(scanResult.Files)}
	for _, file := range scanResult.Files {
		symbols, err := parser.Parse(ctx, file)
		if err != nil {
			if ctx.Err() != nil {
				return repositoryIngest{}, ctx.Err()
			}
			return repositoryIngest{}, errors.New("parse permitted source")
		}
		fileChunks, err := chunker.Chunk(ctx, file, symbols)
		if err != nil {
			if ctx.Err() != nil {
				return repositoryIngest{}, ctx.Err()
			}
			return repositoryIngest{}, errors.New("chunk permitted source")
		}
		stats.symbols += len(symbols)
		stats.chunks += len(fileChunks)
		chunks = append(chunks, fileChunks...)
	}
	corpus, err := source.NewCorpusContext(ctx, scanResult.PolicyVersion, chunks)
	if err != nil {
		return repositoryIngest{}, err
	}
	return repositoryIngest{
		repositoryID: repositoryID,
		corpus:       corpus,
		stats:        stats,
	}, nil
}
