package eval

import "time"

const (
	MaxChecklistBytes int64 = 16 << 10
	MaxReportBytes    int64 = 16 << 20
	maxEligibleBytes  int64 = 1 << 40
	maxEligibleFiles        = 10_000_000
	maxAutoQueries          = 1_000_000
)

type Checklist struct {
	OwnerAuthorized              bool             `json:"owner_authorized"`
	RootReadOnly                 bool             `json:"root_read_only"`
	Task13TaintGatePassed        bool             `json:"task_13_taint_gate_passed"`
	SemanticFixedOff             bool             `json:"semantic_fixed_off"`
	ExternalNetworkProhibited    bool             `json:"external_network_prohibited"`
	OutputReviewedAsAggregates   bool             `json:"output_reviewed_as_aggregates"`
	CacheOutputOutsideRepository bool             `json:"cache_output_outside_repository"`
	RollbackIsCacheDiscard       bool             `json:"rollback_is_cache_discard"`
	Budget                       ChecklistBudgets `json:"budgets"`
}

type ChecklistBudgets struct {
	MaxDurationMilliseconds int64 `json:"max_duration_milliseconds"`
	MaxEligibleBytes        int64 `json:"max_eligible_bytes"`
	MaxEligibleFiles        int   `json:"max_eligible_files"`
	MaxOutputBytes          int64 `json:"max_output_bytes"`
	MaxAutoQueries          int   `json:"max_auto_queries"`
}

func (checklist Checklist) BlockerCount() int {
	blockers := 0
	for _, gate := range []bool{
		checklist.OwnerAuthorized,
		checklist.RootReadOnly,
		checklist.Task13TaintGatePassed,
		checklist.SemanticFixedOff,
		checklist.ExternalNetworkProhibited,
		checklist.OutputReviewedAsAggregates,
		checklist.CacheOutputOutsideRepository,
		checklist.RollbackIsCacheDiscard,
	} {
		if !gate {
			blockers++
		}
	}
	maxDurationMilliseconds := int64((24 * time.Hour) / time.Millisecond)
	if checklist.Budget.MaxDurationMilliseconds <= 0 || checklist.Budget.MaxDurationMilliseconds > maxDurationMilliseconds {
		blockers++
	}
	if checklist.Budget.MaxEligibleBytes <= 0 || checklist.Budget.MaxEligibleBytes > maxEligibleBytes {
		blockers++
	}
	if checklist.Budget.MaxEligibleFiles <= 0 || checklist.Budget.MaxEligibleFiles > maxEligibleFiles {
		blockers++
	}
	if checklist.Budget.MaxOutputBytes <= 0 || checklist.Budget.MaxOutputBytes > MaxReportBytes {
		blockers++
	}
	if checklist.Budget.MaxAutoQueries <= 0 || checklist.Budget.MaxAutoQueries > maxAutoQueries {
		blockers++
	}
	return blockers
}

func (checklist Checklist) EvaluationBudgets() Budgets {
	return Budgets{
		MaxEligibleFiles: checklist.Budget.MaxEligibleFiles,
		MaxEligibleBytes: checklist.Budget.MaxEligibleBytes,
		MaxAutoQueries:   checklist.Budget.MaxAutoQueries,
	}
}

func (checklist Checklist) Duration() time.Duration {
	return time.Duration(checklist.Budget.MaxDurationMilliseconds) * time.Millisecond
}
