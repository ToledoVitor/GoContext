package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"unicode"
	"unicode/utf8"

	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/source"
)

const MaxGoldSetBytes int64 = 1 << 20

var (
	// ErrGoldSet is the sanitized failure returned for every invalid private
	// gold-set payload.
	ErrGoldSet          = errors.New("invalid gold set")
	opaqueCaseIDPattern = regexp.MustCompile(`^case-[a-z0-9][a-z0-9-]{1,63}$`)
)

// GoldSet is an opaque, validated private evaluation input. Its facts are kept
// out of the aggregate report schema.
type GoldSet struct {
	repository string
	scanPolicy string
	cases      []goldCase
}

type goldExpectation string

const (
	goldRelevant   goldExpectation = "relevant"
	goldNoEvidence goldExpectation = "no_evidence"
)

type goldCase struct {
	id          string
	category    QueryCategory
	query       string
	expectation goldExpectation
	judgments   []goldJudgment
}

type goldJudgment struct {
	reference source.Reference
	relevance int
}

type goldSetJSON struct {
	Schema            int            `json:"schema"`
	Repository        string         `json:"repository"`
	ScanPolicyVersion string         `json:"scan_policy_version"`
	Cases             []goldCaseJSON `json:"cases"`
}

type goldCaseJSON struct {
	ID          string             `json:"id"`
	Category    QueryCategory      `json:"category"`
	Query       string             `json:"query"`
	Expectation goldExpectation    `json:"expectation"`
	Judgments   []goldJudgmentJSON `json:"judgments"`
}

type goldJudgmentJSON struct {
	Reference goldReferenceJSON `json:"reference"`
	Relevance int               `json:"relevance"`
}

type goldReferenceJSON struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// ParseGoldSet validates the bounded private schema without echoing any input.
func ParseGoldSet(ctx context.Context, payload []byte, repositoryID, scanPolicy string) (*GoldSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(payload) == 0 || int64(len(payload)) > MaxGoldSetBytes || !utf8.Valid(payload) ||
		!opaqueRepositoryPattern.MatchString(repositoryID) || scanPolicy != ingest.ScanPolicyVersion ||
		!exactUniqueJSONKeys(payload) {
		return nil, ErrGoldSet
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var decoded goldSetJSON
	if err := decoder.Decode(&decoded); err != nil || !decoderAtEOF(decoder) || decoded.Schema != 1 ||
		decoded.Repository != repositoryID || decoded.ScanPolicyVersion != scanPolicy ||
		len(decoded.Cases) < 1 || len(decoded.Cases) > 1000 {
		return nil, ErrGoldSet
	}

	result := &GoldSet{repository: repositoryID, scanPolicy: scanPolicy, cases: make([]goldCase, 0, len(decoded.Cases))}
	seenIDs := make(map[string]struct{}, len(decoded.Cases))
	type categoryQuery struct {
		category QueryCategory
		query    string
	}
	seenQueries := make(map[categoryQuery]struct{}, len(decoded.Cases))
	for _, candidate := range decoded.Cases {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		queryKey := categoryQuery{category: candidate.Category, query: candidate.Query}
		_, duplicateID := seenIDs[candidate.ID]
		_, duplicateQuery := seenQueries[queryKey]
		if candidate.Judgments == nil || !opaqueCaseIDPattern.MatchString(candidate.ID) || duplicateID || duplicateQuery ||
			!validQueryCategory(candidate.Category) || !privateText(candidate.Query, 1, 4096) {
			return nil, ErrGoldSet
		}
		seenIDs[candidate.ID] = struct{}{}
		seenQueries[queryKey] = struct{}{}

		parsed := goldCase{id: candidate.ID, category: candidate.Category, query: candidate.Query, expectation: candidate.Expectation}
		switch candidate.Expectation {
		case goldRelevant:
			if candidate.Category == CategoryNegativeEvidence || len(candidate.Judgments) < 1 || len(candidate.Judgments) > 100 {
				return nil, ErrGoldSet
			}
		case goldNoEvidence:
			if candidate.Category != CategoryNegativeEvidence || len(candidate.Judgments) != 0 {
				return nil, ErrGoldSet
			}
		default:
			return nil, ErrGoldSet
		}

		seenReferences := make(map[source.Reference]struct{}, len(candidate.Judgments))
		for _, judgment := range candidate.Judgments {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			reference := source.Reference{
				Path: judgment.Reference.Path, StartLine: judgment.Reference.StartLine, EndLine: judgment.Reference.EndLine,
			}
			_, duplicate := seenReferences[reference]
			if !reference.Valid() || !privateText(reference.Path, 1, 4096) || judgment.Relevance < 1 || judgment.Relevance > 3 || duplicate {
				return nil, ErrGoldSet
			}
			seenReferences[reference] = struct{}{}
			parsed.judgments = append(parsed.judgments, goldJudgment{reference: reference, relevance: judgment.Relevance})
		}
		result.cases = append(result.cases, parsed)
	}
	return result, nil
}

func privateText(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func exactUniqueJSONKeys(payload []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if !walkUniqueJSONValue(decoder) {
		return false
	}
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}

func walkUniqueJSONValue(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return true
	}
	switch delimiter {
	case '{':
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
			if !walkUniqueJSONValue(decoder) {
				return false
			}
		}
		closing, err := decoder.Token()
		return err == nil && closing == json.Delim('}')
	case '[':
		for decoder.More() {
			if !walkUniqueJSONValue(decoder) {
				return false
			}
		}
		closing, err := decoder.Token()
		return err == nil && closing == json.Delim(']')
	default:
		return false
	}
}
