package eval_test

import (
	"testing"
	"time"

	evaluation "github.com/ToledoVitor/GoContext/internal/eval"
)

func TestChecklistRequiresEveryGateAndBoundedPositiveBudget(t *testing.T) {
	checklist := validChecklist()
	if blockers := checklist.BlockerCount(); blockers != 0 {
		t.Fatalf("valid checklist blockers = %d", blockers)
	}

	tests := []struct {
		name   string
		mutate func(*evaluation.Checklist)
	}{
		{name: "authorization", mutate: func(value *evaluation.Checklist) { value.OwnerAuthorized = false }},
		{name: "read only", mutate: func(value *evaluation.Checklist) { value.RootReadOnly = false }},
		{name: "taint gate", mutate: func(value *evaluation.Checklist) { value.Task13TaintGatePassed = false }},
		{name: "semantic off", mutate: func(value *evaluation.Checklist) { value.SemanticFixedOff = false }},
		{name: "network", mutate: func(value *evaluation.Checklist) { value.ExternalNetworkProhibited = false }},
		{name: "aggregate review", mutate: func(value *evaluation.Checklist) { value.OutputReviewedAsAggregates = false }},
		{name: "outside", mutate: func(value *evaluation.Checklist) { value.CacheOutputOutsideRepository = false }},
		{name: "rollback", mutate: func(value *evaluation.Checklist) { value.RollbackIsCacheDiscard = false }},
		{name: "duration zero", mutate: func(value *evaluation.Checklist) { value.Budget.MaxDurationMilliseconds = 0 }},
		{name: "duration excessive", mutate: func(value *evaluation.Checklist) {
			value.Budget.MaxDurationMilliseconds = int64((24*time.Hour)/time.Millisecond) + 1
		}},
		{name: "files", mutate: func(value *evaluation.Checklist) { value.Budget.MaxEligibleFiles = 0 }},
		{name: "bytes", mutate: func(value *evaluation.Checklist) { value.Budget.MaxEligibleBytes = 0 }},
		{name: "output", mutate: func(value *evaluation.Checklist) { value.Budget.MaxOutputBytes = 0 }},
		{name: "queries", mutate: func(value *evaluation.Checklist) { value.Budget.MaxAutoQueries = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := checklist
			test.mutate(&value)
			if blockers := value.BlockerCount(); blockers == 0 {
				t.Fatal("BlockerCount() = 0, want no-go")
			}
		})
	}
}

func validChecklist() evaluation.Checklist {
	return evaluation.Checklist{
		OwnerAuthorized: true, RootReadOnly: true, Task13TaintGatePassed: true,
		SemanticFixedOff: true, ExternalNetworkProhibited: true, OutputReviewedAsAggregates: true,
		CacheOutputOutsideRepository: true, RollbackIsCacheDiscard: true,
		Budget: evaluation.ChecklistBudgets{
			MaxDurationMilliseconds: 10_000, MaxEligibleBytes: 1 << 20, MaxEligibleFiles: 100,
			MaxOutputBytes: 1 << 20, MaxAutoQueries: 100,
		},
	}
}
