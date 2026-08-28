//go:build windows

package main

import "errors"

var (
	errEvalOutput              = errors.New("evaluation output privacy unsupported")
	errEvalOutputIndeterminate = errors.New("evaluation output durability indeterminate")
)

type evalOutput struct {
	visible bool
}

func prepareEvalOutput(string) (*evalOutput, error) { return nil, errEvalOutput }
func (*evalOutput) absolutePath() string            { return "" }
func (*evalOutput) Write([]byte, int64) error       { return errEvalOutput }
func (*evalOutput) Close() error                    { return nil }
func readPrivateEvalFile(string, int64) ([]byte, error) {
	return nil, errEvalChecklist
}
