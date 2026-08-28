package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	evaluation "github.com/ToledoVitor/GoContext/internal/eval"
	"github.com/ToledoVitor/GoContext/internal/ingest/filesystem"
)

var (
	errEvalChecklist         = errors.New("invalid evaluation checklist")
	errEvalChecklistLocation = errors.New("evaluation checklist location failed")
)

func readEvalChecklist(path string, root *filesystem.OpenedRoot) (evaluation.Checklist, error) {
	if !cleanAbsoluteEvalPath(path) {
		return evaluation.Checklist{}, errEvalChecklist
	}
	payload, err := readPrivateEvalFileOutsideRoot(path, evaluation.MaxChecklistBytes, root)
	if errors.Is(err, errEvalChecklistLocation) {
		return evaluation.Checklist{}, errEvalChecklistLocation
	}
	if err != nil || !exactChecklistJSONObject(payload) {
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

func exactChecklistJSONObject(payload []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	booleanKeys := map[string]struct{}{
		"owner_authorized": {}, "root_read_only": {}, "task_13_taint_gate_passed": {},
		"semantic_fixed_off": {}, "external_network_prohibited": {},
		"output_reviewed_as_aggregates": {}, "cache_output_outside_repository": {},
		"rollback_is_cache_discard": {},
	}
	seen := make(map[string]struct{}, len(booleanKeys)+1)
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
		if _, boolean := booleanKeys[key]; boolean {
			value, err := decoder.Token()
			if _, ok := value.(bool); err != nil || !ok {
				return false
			}
			continue
		}
		if key != "budgets" || !exactChecklistBudgets(decoder) {
			return false
		}
	}
	closing, err := decoder.Token()
	return err == nil && closing == json.Delim('}') && decoderAtEvalEOF(decoder)
}

func exactChecklistBudgets(decoder *json.Decoder) bool {
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return false
	}
	allowed := map[string]struct{}{
		"max_duration_milliseconds": {}, "max_eligible_bytes": {}, "max_eligible_files": {},
		"max_output_bytes": {}, "max_auto_queries": {},
	}
	seen := make(map[string]struct{}, len(allowed))
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok {
			return false
		}
		if _, permitted := allowed[key]; !permitted {
			return false
		}
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
		value, err := decoder.Token()
		if _, ok := value.(json.Number); err != nil || !ok {
			return false
		}
	}
	closing, err := decoder.Token()
	return err == nil && closing == json.Delim('}')
}

func decoderAtEvalEOF(decoder *json.Decoder) bool {
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}

func cleanAbsoluteEvalPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && filepath.Base(path) != "." && filepath.Base(path) != string(filepath.Separator)
}

func openCanonicalEvalRoot(root string) (*filesystem.OpenedRoot, error) {
	if !cleanAbsoluteEvalPath(root) {
		return nil, errEvalChecklist
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || canonical != root {
		return nil, errEvalChecklist
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errEvalChecklist
	}
	opened, err := filesystem.OpenRoot(root)
	if err != nil {
		return nil, errEvalChecklist
	}
	sameIdentity, compareErr := opened.CompareIdentity(info)
	if compareErr != nil || !sameIdentity || !opened.MatchesPath(root) {
		_ = opened.Close()
		return nil, errEvalChecklist
	}
	return opened, nil
}
