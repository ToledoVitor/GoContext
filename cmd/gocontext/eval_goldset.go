package main

import (
	"context"
	"errors"

	evaluation "github.com/ToledoVitor/GoContext/internal/eval"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/ingest/filesystem"
)

var errEvalGoldSet = errors.New("invalid evaluation gold set")

func readEvalGoldSet(ctx context.Context, path, repositoryID string, root *filesystem.OpenedRoot) (*evaluation.GoldSet, error) {
	if !cleanAbsoluteEvalPath(path) {
		return nil, errEvalGoldSet
	}
	payload, err := readPrivateEvalGoldFileOutsideRoot(path, evaluation.MaxGoldSetBytes, root)
	if err != nil {
		return nil, errEvalGoldSet
	}
	goldSet, err := evaluation.ParseGoldSet(ctx, payload, repositoryID, ingest.ScanPolicyVersion)
	if err != nil {
		return nil, errEvalGoldSet
	}
	return goldSet, nil
}
