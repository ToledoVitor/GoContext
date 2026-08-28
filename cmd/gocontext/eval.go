package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	evaluation "github.com/ToledoVitor/GoContext/internal/eval"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/ingest/filesystem"
	"github.com/ToledoVitor/GoContext/internal/ingest/lineparser"
	"github.com/ToledoVitor/GoContext/internal/ingest/symbolchunker"
	searchdomain "github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/search/lexical"
	"github.com/ToledoVitor/GoContext/internal/source"
)

var evalRepositoryPattern = regexp.MustCompile(`^repo-[a-f0-9]{2,64}$`)

var errDuplicateEvalFlag = errors.New("duplicate evaluation flag")

type requiredEvalFlag struct {
	value string
	count int
}

func (value *requiredEvalFlag) String() string { return "" }

func (value *requiredEvalFlag) Set(candidate string) error {
	value.count++
	if value.count != 1 {
		return errDuplicateEvalFlag
	}
	value.value = candidate
	return nil
}

type evalCorpusLoader struct {
	repositoryID string
	chunks       []source.Chunk
}

type evalCLIComposition struct {
	newScanner    func() ingest.Scanner
	newParser     func() ingest.Parser
	newChunker    func() ingest.Chunker
	searchFactory evaluation.SearchFactory
}

func (loader evalCorpusLoader) Load(ctx context.Context, repositoryID string) ([]source.Chunk, error) {
	if err := ctx.Err(); err != nil || repositoryID != loader.repositoryID {
		return nil, context.Canceled
	}
	return append([]source.Chunk(nil), loader.chunks...), nil
}

func runEval(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runEvalWithComposition(ctx, args, stdout, stderr, evalCLIComposition{
		newScanner: func() ingest.Scanner { return filesystem.NewScanner() },
		newParser:  func() ingest.Parser { return lineparser.NewParser() },
		newChunker: func() ingest.Chunker { return symbolchunker.NewChunker() },
		searchFactory: func(repositoryID string, chunks []source.Chunk) (searchdomain.Searcher, error) {
			return lexical.NewSearcher(evalCorpusLoader{repositoryID: repositoryID, chunks: append([]source.Chunk(nil), chunks...)})
		},
	})
}

func runEvalWithComposition(ctx context.Context, args []string, stdout, stderr io.Writer, composition evalCLIComposition) int {
	if len(args) == 0 || args[0] != "inventory" {
		return evalInputFailure(stderr)
	}
	flags := flag.NewFlagSet("eval inventory", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var root, checklistPath, outputPath, repositoryID requiredEvalFlag
	flags.Var(&root, "root", "")
	flags.Var(&checklistPath, "checklist", "")
	flags.Var(&outputPath, "output", "")
	flags.Var(&repositoryID, "repository", "")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || root.count != 1 || checklistPath.count != 1 ||
		outputPath.count != 1 || repositoryID.count != 1 || root.value == "" || checklistPath.value == "" ||
		outputPath.value == "" || !evalRepositoryPattern.MatchString(repositoryID.value) {
		return evalInputFailure(stderr)
	}

	output, err := prepareEvalOutput(outputPath.value)
	if err != nil {
		return evalTrustFailure(stdout, stderr, "output")
	}
	canonicalRoot, rootErr := validateEvalRoot(root.value)
	if rootErr != nil {
		return finishEvalReport(output, evalNoGo(repositoryID.value, evaluation.BlockerRoot, 1), evaluation.MaxReportBytes, stdout, stderr, "root")
	}
	if pathInside(canonicalRoot, output.absolutePath()) {
		_ = output.Close()
		return evalTrustFailure(stdout, stderr, "location")
	}
	if pathInside(canonicalRoot, checklistPath.value) {
		return finishEvalReport(output, evalNoGo(repositoryID.value, evaluation.BlockerLocation, 1), evaluation.MaxReportBytes, stdout, stderr, "location")
	}

	checklist, err := readEvalChecklist(checklistPath.value)
	if err != nil {
		return finishEvalReport(output, evalNoGo(repositoryID.value, evaluation.BlockerChecklist, 1), evaluation.MaxReportBytes, stdout, stderr, "checklist")
	}
	if blockers := checklist.BlockerCount(); blockers > 0 {
		return finishEvalReport(output, evalNoGo(repositoryID.value, evaluation.BlockerChecklist, blockers), evaluation.MaxReportBytes, stdout, stderr, "checklist")
	}

	evaluationContext, cancel := context.WithTimeout(ctx, checklist.Duration())
	defer cancel()
	if composition.newScanner == nil || composition.newParser == nil || composition.newChunker == nil || composition.searchFactory == nil {
		return finishEvalReport(output, evalNoGo(repositoryID.value, evaluation.BlockerIntegrity, 1), checklist.MaxOutputBytes, stdout, stderr, "integrity")
	}
	report, evaluationErr := evaluation.Evaluate(evaluationContext, repositoryID.value, canonicalRoot, evaluation.Dependencies{
		Scanner: composition.newScanner(), Parser: composition.newParser(), Chunker: composition.newChunker(),
		SearchFactory: composition.searchFactory,
	}, checklist.Budgets())
	if evaluationErr != nil {
		return finishEvalReport(output, report, checklist.MaxOutputBytes, stdout, stderr, evalReportErrorCategory(report))
	}
	return finishEvalReport(output, report, checklist.MaxOutputBytes, stdout, stderr, "")
}

func evalNoGo(repositoryID string, blocker evaluation.Blocker, count int) evaluation.Report {
	report := evaluation.EmptyReport(repositoryID, evaluation.DecisionNoGo)
	report.Blockers[blocker] = count
	return report
}

func finishEvalReport(
	output *evalOutput,
	report evaluation.Report,
	maxBytes int64,
	stdout, stderr io.Writer,
	errorCategory string,
) int {
	payload, err := evaluation.MarshalValidated(report)
	if err == nil {
		err = output.Write(payload, maxBytes)
	}
	closeErr := output.Close()
	if err != nil || closeErr != nil {
		fmt.Fprintf(stdout, "evaluation: %s\n", report.Decision)
		if err == errEvalOutputIndeterminate || closeErr != nil && output.visible {
			fmt.Fprintln(stderr, "evaluation error: output_indeterminate")
		} else {
			fmt.Fprintln(stderr, "evaluation error: output")
		}
		return 1
	}
	fmt.Fprintf(stdout, "evaluation: %s\n", report.Decision)
	if errorCategory != "" {
		fmt.Fprintf(stderr, "evaluation error: %s\n", errorCategory)
		return 1
	}
	return 0
}

func evalInputFailure(stderr io.Writer) int {
	fmt.Fprintln(stderr, "evaluation error: input")
	return 2
}

func evalTrustFailure(stdout, stderr io.Writer, category string) int {
	fmt.Fprintln(stdout, "evaluation: no-go")
	fmt.Fprintf(stderr, "evaluation error: %s\n", category)
	return 1
}

func evalReportErrorCategory(report evaluation.Report) string {
	ordered := []struct {
		blocker  evaluation.Blocker
		category string
	}{
		{evaluation.BlockerBudget, "budget"},
		{evaluation.BlockerScan, "scan"},
		{evaluation.BlockerIntegrity, "integrity"},
		{evaluation.BlockerRetrieval, "retrieval"},
		{evaluation.BlockerCanceled, "canceled"},
	}
	for _, candidate := range ordered {
		if report.Blockers[candidate.blocker] > 0 {
			return candidate.category
		}
	}
	return "integrity"
}

func pathInside(root, target string) bool {
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return false
	}
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
