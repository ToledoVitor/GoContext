//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	evaluation "github.com/ToledoVitor/GoContext/internal/eval"
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/ingest/filesystem"
	"github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/source"
	"github.com/ToledoVitor/GoContext/internal/testsupport/taintcheck"
)

func TestRunEvalInventoryWritesSyntheticAggregateOnlyReport(t *testing.T) {
	fixture := newEvalCLIFixture(t)
	writeEvalCLIFile(t, fixture.root, "private-CANARY/SymbolPathCANARY.ts", "export function ExactSymbolCANARY() { return 'query-CANARY https://user-CANARY.invalid' }\n")
	writeEvalCLIFile(t, fixture.root, ".env.ts", "export const password = 'hard-denied-CANARY'\n")
	writeEvalCLIFile(t, fixture.root, "notes.customer-CANARY", "unsupported-content-CANARY")
	t.Setenv("OPENAI_API_KEY", "hostile-environment-CANARY")
	t.Setenv("GOCONTEXT_EMBEDDING_BASE_URL", "https://network-CANARY.invalid")

	var stdout, stderr bytes.Buffer
	exitCode := run(evalCLIArgs(fixture), &stdout, &stderr)
	if exitCode != 0 || stdout.String() != "evaluation: go\n" || stderr.Len() != 0 {
		t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
	payload, err := os.ReadFile(fixture.output)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(fixture.output)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("output mode = %#o, want 0600", info.Mode().Perm())
	}
	forbidden := []string{
		fixture.root, filepath.Base(fixture.root), fixture.checklist, fixture.output,
		"private-CANARY", "SymbolPathCANARY", "ExactSymbolCANARY", "query-CANARY",
		"user-CANARY", "hard-denied-CANARY", "customer-CANARY", "unsupported-content-CANARY",
		"hostile-environment-CANARY", "network-CANARY",
	}
	for _, sink := range [][]byte{payload, stdout.Bytes(), stderr.Bytes()} {
		result := taintcheck.Scan(sink, forbidden)
		if !result.Complete || result.Found {
			t.Fatalf("taint result = %#v for %q", result, sink)
		}
	}
	var report evaluation.Report
	if err := json.Unmarshal(payload, &report); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if report.Repository != "repo-a1" || report.Decision != evaluation.DecisionGo || report.Inventory.IncludedFiles != 1 {
		t.Fatalf("report = %#v", report)
	}
	structured := taintcheck.ScanValue(report, forbidden)
	debug := taintcheck.Scan([]byte(fmt.Sprintf("%#v", report)), forbidden)
	if !structured.Complete || structured.Found || !debug.Complete || debug.Found {
		t.Fatalf("structured/debug taint = %#v/%#v", structured, debug)
	}
	if report.Inventory.ExcludedByCategory["security"] != 1 || report.Inventory.UnsupportedExtensions["<other>"].Files != 1 {
		t.Fatalf("inventory = %#v", report.Inventory)
	}
}

func TestRunEvalInventoryWithoutGoldSetPreservesOwnerReadOnlyChecklist(t *testing.T) {
	fixture := newEvalCLIFixture(t)
	if err := os.Chmod(fixture.checklist, 0o400); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	exitCode := run(evalCLIArgs(fixture), &stdout, &stderr)
	if exitCode != 0 || stdout.String() != "evaluation: go\n" || stderr.Len() != 0 {
		t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
}

func TestRunEvalInventoryAcceptsPrivateGoldSetWithoutPublishingPrivateFacts(t *testing.T) {
	fixture := newEvalCLIFixture(t)
	writeEvalCLIFile(t, fixture.root, "safe.py", "def Match():\n")
	goldPath := filepath.Join(filepath.Dir(fixture.checklist), "gold-path-CANARY.json")
	writeEvalGoldSet(t, goldPath, `gold-query-CANARY`)
	t.Setenv("OPENAI_API_KEY", "hostile-gold-environment-CANARY")
	t.Setenv("GOCONTEXT_EMBEDDING_BASE_URL", "https://gold-network-CANARY.invalid")

	var stdout, stderr bytes.Buffer
	exitCode := run(append(evalCLIArgs(fixture), "--gold-set", goldPath), &stdout, &stderr)
	if exitCode != 0 || stdout.String() != "evaluation: go\n" || stderr.Len() != 0 {
		t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
	payload, err := os.ReadFile(fixture.output)
	if err != nil {
		t.Fatal(err)
	}
	var report evaluation.Report
	if err := json.Unmarshal(payload, &report); err != nil {
		t.Fatal(err)
	}
	concept := report.Retrieval.Categories[evaluation.CategoryConcept]
	if report.Schema != 2 || concept.Status != evaluation.StatusEvaluated || concept.QueryCount != 1 ||
		report.Retrieval.Categories[evaluation.CategoryExactSymbol].Status != evaluation.StatusNotEvaluated ||
		report.CapabilityGaps[evaluation.CategoryConcept] != evaluation.StatusEvaluated ||
		report.Limitations[evaluation.LimitationAutomaticSymbols] != 0 {
		t.Fatalf("report = %#v", report)
	}
	forbidden := []string{goldPath, "gold-path-CANARY", "gold-query-CANARY", "safe.py", "Match", "hostile-gold-environment-CANARY", "gold-network-CANARY"}
	for _, sink := range [][]byte{payload, stdout.Bytes(), stderr.Bytes(), []byte(fmt.Sprintf("%#v", report))} {
		result := taintcheck.Scan(sink, forbidden)
		if !result.Complete || result.Found {
			t.Fatalf("taint result = %#v for %q", result, sink)
		}
	}
}

func TestRunEvalInventoryRejectsUntrustedGoldSetBeforePipeline(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*testing.T, evalCLIFixture) string
	}{
		{name: "relative", configure: func(*testing.T, evalCLIFixture) string { return "gold.json" }},
		{name: "inside root", configure: func(t *testing.T, fixture evalCLIFixture) string {
			path := filepath.Join(fixture.root, "gold.json")
			writeEvalGoldSet(t, path, "private-query-CANARY")
			return path
		}},
		{name: "symlink", configure: func(t *testing.T, fixture evalCLIFixture) string {
			target := filepath.Join(filepath.Dir(fixture.checklist), "gold-target.json")
			writeEvalGoldSet(t, target, "private-query-CANARY")
			alias := filepath.Join(filepath.Dir(fixture.checklist), "gold-alias.json")
			if err := os.Symlink(target, alias); err != nil {
				t.Fatal(err)
			}
			return alias
		}},
		{name: "hardlink", configure: func(t *testing.T, fixture evalCLIFixture) string {
			target := filepath.Join(filepath.Dir(fixture.checklist), "gold-target.json")
			writeEvalGoldSet(t, target, "private-query-CANARY")
			alias := filepath.Join(filepath.Dir(fixture.checklist), "gold-hardlink.json")
			if err := os.Link(target, alias); err != nil {
				t.Fatal(err)
			}
			return alias
		}},
		{name: "permissive", configure: func(t *testing.T, fixture evalCLIFixture) string {
			path := filepath.Join(filepath.Dir(fixture.checklist), "gold.json")
			writeEvalGoldSet(t, path, "private-query-CANARY")
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{name: "owner read-only", configure: func(t *testing.T, fixture evalCLIFixture) string {
			path := filepath.Join(filepath.Dir(fixture.checklist), "gold.json")
			writeEvalGoldSet(t, path, "private-query-CANARY")
			if err := os.Chmod(path, 0o400); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{name: "whitespace query", configure: func(t *testing.T, fixture evalCLIFixture) string {
			path := filepath.Join(filepath.Dir(fixture.checklist), "gold.json")
			writeEvalGoldSet(t, path, " \t ")
			return path
		}},
		{name: "malformed schema", configure: func(t *testing.T, fixture evalCLIFixture) string {
			path := filepath.Join(filepath.Dir(fixture.checklist), "gold.json")
			if err := os.WriteFile(path, []byte(`{"private":"private-query-CANARY"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEvalCLIFixture(t)
			goldPath := test.configure(t, fixture)
			constructed, searched := 0, 0
			composition := evalCLIComposition{
				newScanner:    func() ingest.Scanner { constructed++; return nil },
				newParser:     func() ingest.Parser { constructed++; return nil },
				newChunker:    func() ingest.Chunker { constructed++; return nil },
				searchFactory: func(string, []source.Chunk) (search.Searcher, error) { searched++; return nil, nil },
			}
			var stdout, stderr bytes.Buffer
			args := append(evalCLIArgs(fixture)[1:], "--gold-set", goldPath)
			exitCode := runEvalWithComposition(context.Background(), args, &stdout, &stderr, composition)
			if exitCode != 1 || stdout.String() != "evaluation: no-go\n" || stderr.String() != "evaluation error: gold_set\n" || constructed != 0 || searched != 0 {
				t.Fatalf("exit/stdout/stderr/constructed/searched = %d/%q/%q/%d/%d", exitCode, stdout.String(), stderr.String(), constructed, searched)
			}
			payload, err := os.ReadFile(fixture.output)
			if err != nil {
				t.Fatal(err)
			}
			var report evaluation.Report
			if err := json.Unmarshal(payload, &report); err != nil || report.Blockers[evaluation.BlockerGoldSet] != 1 {
				t.Fatalf("report/error = %#v/%v", report, err)
			}
			result := taintcheck.Scan(payload, []string{goldPath, "private-query-CANARY"})
			if !result.Complete || result.Found {
				t.Fatalf("taint = %#v", result)
			}
		})
	}
}

func TestRunEvalInventoryWritesNoGoWithoutScanningForInvalidChecklist(t *testing.T) {
	fixture := newEvalCLIFixture(t)
	checklist := validEvalChecklist()
	checklist.OwnerAuthorized = false
	writeEvalChecklist(t, fixture.checklist, checklist)

	var stdout, stderr bytes.Buffer
	exitCode := run(evalCLIArgs(fixture), &stdout, &stderr)
	if exitCode != 1 || stdout.String() != "evaluation: no-go\n" || stderr.String() != "evaluation error: checklist\n" {
		t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
	var report evaluation.Report
	payload, err := os.ReadFile(fixture.output)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &report); err != nil {
		t.Fatal(err)
	}
	if report.Decision != evaluation.DecisionNoGo || report.Blockers[evaluation.BlockerChecklist] != 1 || report.Inventory.IncludedFiles != 0 {
		t.Fatalf("report = %#v", report)
	}
}

func TestRunEvalInventoryRejectsChecklistSchemaVariantsBeforeScanning(t *testing.T) {
	tests := []struct {
		name    string
		payload func(evaluation.Checklist) []byte
		mode    os.FileMode
	}{
		{name: "unknown", payload: func(value evaluation.Checklist) []byte {
			payload, _ := json.Marshal(value)
			return append(payload[:len(payload)-1], []byte(`,"unknown":true}`)...)
		}, mode: 0o600},
		{name: "duplicate", payload: func(value evaluation.Checklist) []byte {
			payload, _ := json.Marshal(value)
			return append(payload[:len(payload)-1], []byte(`,"owner_authorized":true}`)...)
		}, mode: 0o600},
		{name: "case variant", payload: func(value evaluation.Checklist) []byte {
			payload, _ := json.Marshal(value)
			return bytes.Replace(payload, []byte(`"owner_authorized"`), []byte(`"Owner_Authorized"`), 1)
		}, mode: 0o600},
		{name: "nested unknown", payload: func(value evaluation.Checklist) []byte {
			payload, _ := json.Marshal(value)
			return bytes.Replace(payload, []byte(`"max_auto_queries":100`), []byte(`"max_auto_queries":100,"unknown":1`), 1)
		}, mode: 0o600},
		{name: "nested duplicate", payload: func(value evaluation.Checklist) []byte {
			payload, _ := json.Marshal(value)
			return bytes.Replace(payload, []byte(`"max_auto_queries":100`), []byte(`"max_auto_queries":100,"max_auto_queries":100`), 1)
		}, mode: 0o600},
		{name: "nested case variant", payload: func(value evaluation.Checklist) []byte {
			payload, _ := json.Marshal(value)
			return bytes.Replace(payload, []byte(`"max_auto_queries"`), []byte(`"Max_Auto_Queries"`), 1)
		}, mode: 0o600},
		{name: "boolean wrong type", payload: func(value evaluation.Checklist) []byte {
			payload, _ := json.Marshal(value)
			return bytes.Replace(payload, []byte(`"owner_authorized":true`), []byte(`"owner_authorized":1`), 1)
		}, mode: 0o600},
		{name: "budgets wrong type", payload: func(value evaluation.Checklist) []byte {
			payload, _ := json.Marshal(value)
			start := bytes.Index(payload, []byte(`"budgets":`))
			if start < 0 {
				return payload
			}
			return append(append([]byte(nil), payload[:start]...), []byte(`"budgets":true}`)...)
		}, mode: 0o600},
		{name: "budget wrong type", payload: func(value evaluation.Checklist) []byte {
			payload, _ := json.Marshal(value)
			return bytes.Replace(payload, []byte(`"max_auto_queries":100`), []byte(`"max_auto_queries":"100"`), 1)
		}, mode: 0o600},
		{name: "trailing", payload: func(value evaluation.Checklist) []byte {
			payload, _ := json.Marshal(value)
			return append(payload, []byte(` {}`)...)
		}, mode: 0o600},
		{name: "permissive", payload: func(value evaluation.Checklist) []byte {
			payload, _ := json.Marshal(value)
			return payload
		}, mode: 0o644},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEvalCLIFixtureWithoutChecklist(t)
			if err := os.WriteFile(fixture.checklist, test.payload(validEvalChecklist()), test.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(fixture.checklist, test.mode); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			exitCode := run(evalCLIArgs(fixture), &stdout, &stderr)
			if exitCode != 1 || stdout.String() != "evaluation: no-go\n" || stderr.String() != "evaluation error: checklist\n" {
				t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
			}
			payload, err := os.ReadFile(fixture.output)
			if err != nil {
				t.Fatal(err)
			}
			var report evaluation.Report
			if err := json.Unmarshal(payload, &report); err != nil || report.Blockers[evaluation.BlockerChecklist] == 0 {
				t.Fatalf("report/error = %#v/%v", report, err)
			}
		})
	}
}

func TestRunEvalInventoryTrustFailuresDoNotWriteUnsafeOutput(t *testing.T) {
	t.Run("output inside root", func(t *testing.T) {
		fixture := newEvalCLIFixture(t)
		fixture.output = filepath.Join(fixture.root, "report.json")
		var stdout, stderr bytes.Buffer
		exitCode := run(evalCLIArgs(fixture), &stdout, &stderr)
		if exitCode != 1 || stdout.String() != "evaluation: no-go\n" || stderr.String() != "evaluation error: location\n" {
			t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
		}
		if _, err := os.Lstat(fixture.output); !os.IsNotExist(err) {
			t.Fatalf("unsafe output exists/error = %v", err)
		}
	})

	t.Run("checklist inside root", func(t *testing.T) {
		fixture := newEvalCLIFixture(t)
		inside := filepath.Join(fixture.root, "gate.json")
		writeEvalChecklist(t, inside, validEvalChecklist())
		fixture.checklist = inside
		var stdout, stderr bytes.Buffer
		exitCode := run(evalCLIArgs(fixture), &stdout, &stderr)
		if exitCode != 1 || stderr.String() != "evaluation error: location\n" {
			t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
		}
		var report evaluation.Report
		payload, err := os.ReadFile(fixture.output)
		if err != nil || json.Unmarshal(payload, &report) != nil || report.Blockers[evaluation.BlockerLocation] != 1 {
			t.Fatalf("report/error = %#v/%v", report, err)
		}
	})

	t.Run("output symlink", func(t *testing.T) {
		fixture := newEvalCLIFixture(t)
		target := filepath.Join(filepath.Dir(fixture.output), "target")
		if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, fixture.output); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		exitCode := run(evalCLIArgs(fixture), &stdout, &stderr)
		if exitCode != 1 || stdout.String() != "evaluation: no-go\n" || stderr.String() != "evaluation error: output\n" {
			t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
		}
		payload, err := os.ReadFile(target)
		if err != nil || string(payload) != "preserve" {
			t.Fatalf("target = %q, error = %v", payload, err)
		}
	})

	t.Run("permissive output parent", func(t *testing.T) {
		fixture := newEvalCLIFixture(t)
		if err := os.Chmod(filepath.Dir(fixture.output), 0o755); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		exitCode := run(evalCLIArgs(fixture), &stdout, &stderr)
		if exitCode != 1 || stderr.String() != "evaluation error: output\n" {
			t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
		}
		if _, err := os.Lstat(fixture.output); !os.IsNotExist(err) {
			t.Fatalf("output exists/error = %v", err)
		}
	})

	t.Run("writable output ancestor", func(t *testing.T) {
		fixture := newEvalCLIFixture(t)
		unsafeAncestor := filepath.Join(filepath.Dir(fixture.root), "unsafe-output")
		privateParent := filepath.Join(unsafeAncestor, "private")
		if err := os.MkdirAll(privateParent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(unsafeAncestor, 0o777); err != nil {
			t.Fatal(err)
		}
		fixture.output = filepath.Join(privateParent, "report.json")
		var stdout, stderr bytes.Buffer
		exitCode := run(evalCLIArgs(fixture), &stdout, &stderr)
		if exitCode != 1 || stdout.String() != "evaluation: no-go\n" || stderr.String() != "evaluation error: output\n" {
			t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
		}
		if _, err := os.Lstat(fixture.output); !os.IsNotExist(err) {
			t.Fatalf("output exists/error = %v", err)
		}
	})
}

func TestRunEvalInventoryRootAndChecklistPathFailuresWriteSanitizedNoGo(t *testing.T) {
	t.Run("root symlink", func(t *testing.T) {
		fixture := newEvalCLIFixture(t)
		alias := filepath.Join(filepath.Dir(fixture.root), "root-alias-CANARY")
		if err := os.Symlink(fixture.root, alias); err != nil {
			t.Fatal(err)
		}
		fixture.root = alias
		assertEvalCLINoGo(t, fixture, "root", evaluation.BlockerRoot)
	})

	t.Run("checklist symlink", func(t *testing.T) {
		fixture := newEvalCLIFixture(t)
		alias := filepath.Join(filepath.Dir(fixture.checklist), "gate-alias-CANARY.json")
		if err := os.Symlink(fixture.checklist, alias); err != nil {
			t.Fatal(err)
		}
		fixture.checklist = alias
		assertEvalCLINoGo(t, fixture, "checklist", evaluation.BlockerChecklist)
	})

	t.Run("permissive checklist parent", func(t *testing.T) {
		fixture := newEvalCLIFixture(t)
		if err := os.Chmod(filepath.Dir(fixture.checklist), 0o755); err != nil {
			t.Fatal(err)
		}
		assertEvalCLINoGo(t, fixture, "checklist", evaluation.BlockerChecklist)
	})

	t.Run("writable checklist ancestor", func(t *testing.T) {
		fixture := newEvalCLIFixture(t)
		unsafeAncestor := filepath.Join(filepath.Dir(fixture.root), "unsafe-checklist")
		privateParent := filepath.Join(unsafeAncestor, "private")
		if err := os.MkdirAll(privateParent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(unsafeAncestor, 0o777); err != nil {
			t.Fatal(err)
		}
		fixture.checklist = filepath.Join(privateParent, "gate.json")
		writeEvalChecklist(t, fixture.checklist, validEvalChecklist())
		assertEvalCLINoGo(t, fixture, "checklist", evaluation.BlockerChecklist)
	})
}

func TestRunEvalInventoryPreflightFailuresConstructNoPipelineOrSearch(t *testing.T) {
	constructed, searched := 0, 0
	composition := evalCLIComposition{
		newScanner: func() ingest.Scanner { constructed++; return nil },
		newParser:  func() ingest.Parser { constructed++; return nil },
		newChunker: func() ingest.Chunker { constructed++; return nil },
		searchFactory: func(string, []source.Chunk) (search.Searcher, error) {
			searched++
			return nil, nil
		},
	}
	tests := []struct {
		name      string
		configure func(*testing.T, *evalCLIFixture)
	}{
		{name: "checklist", configure: func(t *testing.T, fixture *evalCLIFixture) {
			checklist := validEvalChecklist()
			checklist.SemanticFixedOff = false
			writeEvalChecklist(t, fixture.checklist, checklist)
		}},
		{name: "root", configure: func(t *testing.T, fixture *evalCLIFixture) {
			alias := filepath.Join(filepath.Dir(fixture.root), "pipeline-root-alias")
			if err := os.Symlink(fixture.root, alias); err != nil {
				t.Fatal(err)
			}
			fixture.root = alias
		}},
		{name: "output", configure: func(t *testing.T, fixture *evalCLIFixture) {
			if err := os.Chmod(filepath.Dir(fixture.output), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEvalCLIFixture(t)
			test.configure(t, &fixture)
			var stdout, stderr bytes.Buffer
			exitCode := runEvalWithComposition(context.Background(), evalCLIArgs(fixture)[1:], &stdout, &stderr, composition)
			if exitCode != 1 || constructed != 0 || searched != 0 {
				t.Fatalf("exit/constructed/searched = %d/%d/%d", exitCode, constructed, searched)
			}
		})
	}
}

func TestRunEvalInventoryRejectsDuplicateExplicitFlagsWithoutWriting(t *testing.T) {
	fixture := newEvalCLIFixture(t)
	args := append(evalCLIArgs(fixture), "--root", fixture.root)
	var stdout, stderr bytes.Buffer
	exitCode := run(args, &stdout, &stderr)
	if exitCode != 2 || stdout.Len() != 0 || stderr.String() != "evaluation error: input\n" {
		t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(fixture.output); !os.IsNotExist(err) {
		t.Fatalf("output exists/error = %v", err)
	}
}

func TestRunEvalInventoryRejectsDuplicateGoldSetFlagWithoutWriting(t *testing.T) {
	fixture := newEvalCLIFixture(t)
	goldPath := filepath.Join(filepath.Dir(fixture.checklist), "gold.json")
	writeEvalGoldSet(t, goldPath, "private query")
	args := append(evalCLIArgs(fixture), "--gold-set", goldPath, "--gold-set", goldPath)
	var stdout, stderr bytes.Buffer
	exitCode := run(args, &stdout, &stderr)
	if exitCode != 2 || stdout.Len() != 0 || stderr.String() != "evaluation error: input\n" {
		t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(fixture.output); !os.IsNotExist(err) {
		t.Fatalf("output exists/error = %v", err)
	}
}

func TestRunEvalInventoryScansRetainedApprovedRootAfterPathReplacement(t *testing.T) {
	fixture := newEvalCLIFixture(t)
	writeEvalCLIFile(t, fixture.root, "approved.py", "def Approved():\n    return 1\n")
	replacement := filepath.Join(filepath.Dir(fixture.root), "replacement")
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	writeEvalCLIFile(t, replacement, "substituted.py", "def Substituted():\n    return 1\n")
	writeEvalCLIFile(t, replacement, "second.py", "def Second():\n    return 2\n")
	retained := fixture.root + "-retained"
	composition := defaultEvalCLIComposition()
	composition.afterOpenRoot = func() error {
		if err := os.Rename(fixture.root, retained); err != nil {
			return err
		}
		return os.Rename(replacement, fixture.root)
	}

	var stdout, stderr bytes.Buffer
	exitCode := runEvalWithComposition(context.Background(), evalCLIArgs(fixture)[1:], &stdout, &stderr, composition)
	if exitCode != 0 || stdout.String() != "evaluation: go\n" || stderr.Len() != 0 {
		t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
	var report evaluation.Report
	payload, err := os.ReadFile(fixture.output)
	if err != nil || json.Unmarshal(payload, &report) != nil {
		t.Fatalf("report read/decode error = %v", err)
	}
	if report.Inventory.IncludedFiles != 1 {
		t.Fatalf("included files = %d, want retained tree's one file", report.Inventory.IncludedFiles)
	}
}

type typedNilEvalParser struct{}

func (*typedNilEvalParser) Parse(context.Context, source.File) ([]source.Symbol, error) {
	return nil, nil
}

type typedNilEvalChunker struct{}

func (*typedNilEvalChunker) Chunk(context.Context, source.File, []source.Symbol) ([]source.Chunk, error) {
	return nil, nil
}

func TestRunEvalInventoryRejectsTypedNilDependenciesBeforeEvaluation(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*evalCLIComposition)
	}{
		{name: "scanner", configure: func(composition *evalCLIComposition) {
			var scanner *filesystem.Scanner
			composition.newScanner = func() ingest.Scanner { return scanner }
		}},
		{name: "parser", configure: func(composition *evalCLIComposition) {
			var parser *typedNilEvalParser
			composition.newParser = func() ingest.Parser { return parser }
		}},
		{name: "chunker", configure: func(composition *evalCLIComposition) {
			var chunker *typedNilEvalChunker
			composition.newChunker = func() ingest.Chunker { return chunker }
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newEvalCLIFixture(t)
			composition := defaultEvalCLIComposition()
			test.configure(&composition)
			beforeDescriptors := evalOpenDescriptorCount(t)
			var stdout, stderr bytes.Buffer
			exitCode := runEvalWithComposition(context.Background(), evalCLIArgs(fixture)[1:], &stdout, &stderr, composition)
			if exitCode != 1 || stdout.String() != "evaluation: no-go\n" || stderr.String() != "evaluation error: integrity\n" {
				t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
			}
			var report evaluation.Report
			payload, err := os.ReadFile(fixture.output)
			if err != nil || json.Unmarshal(payload, &report) != nil || report.Blockers[evaluation.BlockerIntegrity] != 1 {
				t.Fatalf("report/error = %#v/%v", report, err)
			}
			afterDescriptors := evalOpenDescriptorCount(t)
			if beforeDescriptors >= 0 && afterDescriptors != beforeDescriptors {
				t.Fatalf("open descriptor count before/after = %d/%d", beforeDescriptors, afterDescriptors)
			}
		})
	}
}

func evalOpenDescriptorCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

type evalCLIFixture struct {
	root      string
	checklist string
	output    string
}

func newEvalCLIFixture(t *testing.T) evalCLIFixture {
	t.Helper()
	fixture := newEvalCLIFixtureWithoutChecklist(t)
	writeEvalChecklist(t, fixture.checklist, validEvalChecklist())
	return fixture
}

func newEvalCLIFixtureWithoutChecklist(t *testing.T) evalCLIFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "root-path-CANARY")
	checklistDirectory := filepath.Join(base, "checklist")
	outputDirectory := filepath.Join(base, "output")
	for _, directory := range []string{root, checklistDirectory, outputDirectory} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return evalCLIFixture{
		root: root, checklist: filepath.Join(checklistDirectory, "gate.json"), output: filepath.Join(outputDirectory, "report.json"),
	}
}

func validEvalChecklist() evaluation.Checklist {
	return evaluation.Checklist{
		OwnerAuthorized: true, RootReadOnly: true, Task13TaintGatePassed: true,
		SemanticFixedOff: true, ExternalNetworkProhibited: true, OutputReviewedAsAggregates: true,
		CacheOutputOutsideRepository: true, RollbackIsCacheDiscard: true,
		Budget: evaluation.ChecklistBudgets{
			MaxDurationMilliseconds: 30_000, MaxEligibleBytes: 1 << 20, MaxEligibleFiles: 100,
			MaxOutputBytes: 1 << 20, MaxAutoQueries: 100,
		},
	}
}

func writeEvalChecklist(t *testing.T, target string, checklist evaluation.Checklist) {
	t.Helper()
	payload, err := json.Marshal(checklist)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeEvalGoldSet(t *testing.T, target, query string) {
	t.Helper()
	payload := []byte(fmt.Sprintf(`{"schema":1,"repository":"repo-a1","scan_policy_version":%q,"cases":[{"id":"case-a1","category":"concept","query":%q,"expectation":"relevant","judgments":[{"reference":{"path":"safe.py","start_line":1,"end_line":1},"relevance":3}]}]}`, ingest.ScanPolicyVersion, query))
	if err := os.WriteFile(target, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeEvalCLIFile(t *testing.T, root, relative, content string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func evalCLIArgs(fixture evalCLIFixture) []string {
	return []string{
		"eval", "inventory", "--root", fixture.root, "--checklist", fixture.checklist,
		"--output", fixture.output, "--repository", "repo-a1",
	}
}

func assertEvalCLINoGo(t *testing.T, fixture evalCLIFixture, category string, blocker evaluation.Blocker) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	exitCode := run(evalCLIArgs(fixture), &stdout, &stderr)
	if exitCode != 1 || stdout.String() != "evaluation: no-go\n" || stderr.String() != "evaluation error: "+category+"\n" {
		t.Fatalf("exit/stdout/stderr = %d/%q/%q", exitCode, stdout.String(), stderr.String())
	}
	payload, err := os.ReadFile(fixture.output)
	if err != nil {
		t.Fatal(err)
	}
	var report evaluation.Report
	if err := json.Unmarshal(payload, &report); err != nil || report.Blockers[blocker] == 0 {
		t.Fatalf("report/error = %#v/%v", report, err)
	}
	forbidden := []string{fixture.root, fixture.checklist, fixture.output, "CANARY"}
	result := taintcheck.Scan(payload, forbidden)
	if !result.Complete || result.Found {
		t.Fatalf("taint result = %#v", result)
	}
}
