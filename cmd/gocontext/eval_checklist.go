package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	evaluation "github.com/ToledoVitor/GoContext/internal/eval"
)

var errEvalChecklist = errors.New("invalid evaluation checklist")

func readEvalChecklist(path string) (evaluation.Checklist, error) {
	if !cleanAbsoluteEvalPath(path) {
		return evaluation.Checklist{}, errEvalChecklist
	}
	payload, err := readPrivateEvalFile(path, evaluation.MaxChecklistBytes)
	if err != nil || !uniqueFlatJSONObject(payload) {
		return evaluation.Checklist{}, errEvalChecklist
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var checklist evaluation.Checklist
	if err := decoder.Decode(&checklist); err != nil || !decoderAtEvalEOF(decoder) {
		return evaluation.Checklist{}, errEvalChecklist
	}
	return checklist, nil
}

func uniqueFlatJSONObject(payload []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok {
			return false
		}
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
		value, err := decoder.Token()
		if err != nil {
			return false
		}
		switch value.(type) {
		case bool, json.Number:
		default:
			return false
		}
	}
	closing, err := decoder.Token()
	return err == nil && closing == json.Delim('}') && decoderAtEvalEOF(decoder)
}

func decoderAtEvalEOF(decoder *json.Decoder) bool {
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}

func cleanAbsoluteEvalPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && filepath.Base(path) != "." && filepath.Base(path) != string(filepath.Separator)
}

func validateEvalRoot(root string) (string, error) {
	if !cleanAbsoluteEvalPath(root) {
		return "", errEvalChecklist
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || canonical != root {
		return "", errEvalChecklist
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errEvalChecklist
	}
	return canonical, nil
}
