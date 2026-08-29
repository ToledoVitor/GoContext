//go:build windows

package main

import (
	"errors"

	"github.com/ToledoVitor/GoContext/internal/ingest/filesystem"
)

var (
	errEvalOutput              = errors.New("evaluation output privacy unsupported")
	errEvalOutputIndeterminate = errors.New("evaluation output durability indeterminate")
)

type evalOutput struct {
	visible bool
}

func prepareEvalOutput(string) (*evalOutput, error) { return nil, errEvalOutput }
func (*evalOutput) requireOutsideRoot(*filesystem.OpenedRoot) error {
	return errEvalOutput
}
func (*evalOutput) Write([]byte, int64) error { return errEvalOutput }
func (*evalOutput) Close() error              { return nil }
func readPrivateEvalFileOutsideRoot(string, int64, *filesystem.OpenedRoot) ([]byte, error) {
	return nil, errEvalChecklist
}
func readPrivateEvalGoldFileOutsideRoot(string, int64, *filesystem.OpenedRoot) ([]byte, error) {
	return nil, errEvalChecklist
}
