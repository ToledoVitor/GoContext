package taintcheck

import "testing"

type recursiveValueFixture struct {
	Public  any
	private string
}

type panickingStringer struct{}

func (panickingStringer) String() string {
	panic("String must not be called")
}

func TestScanValueFindsNestedExportedValuesAndTerminatesCycles(t *testing.T) {
	const canary = "STRUCTURED_VALUE_CANARY_TASK13"
	cycle := map[string]any{}
	cycle["self"] = cycle
	cycle["fixture"] = recursiveValueFixture{
		Public:  []any{"safe", []byte(canary)},
		private: "UNEXPORTED_VALUE_MUST_NOT_LEAK",
	}
	cycle["stringer"] = panickingStringer{}

	result := ScanValue(cycle, []string{canary})
	if !result.Complete || !result.Found || result.Match.Canary != canary {
		t.Fatalf("ScanValue() = %#v, want complete nested match", result)
	}
}

func TestScanValueDoesNotExposeUnexportedFieldsOrPanicOnCycles(t *testing.T) {
	const privateCanary = "UNEXPORTED_VALUE_MUST_NOT_LEAK"
	cycle := &recursiveValueFixture{private: privateCanary}
	cycle.Public = cycle

	result := ScanValue(cycle, []string{privateCanary})
	if !result.Complete || result.Found {
		t.Fatalf("ScanValue(private cycle) = %#v, want complete no-match", result)
	}
}
