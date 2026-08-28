// Package eval provides a provider-neutral, aggregate-only local evaluation
// module. Query text, source references, symbols, chunks, and paths remain
// private implementation data and never occur in Report.
package eval

import (
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/source"
)

const SchemaVersion = 1

type Decision string

const (
	DecisionGo   Decision = "go"
	DecisionNoGo Decision = "no-go"
)

type Blocker string

const (
	BlockerChecklist Blocker = "checklist"
	BlockerRoot      Blocker = "root"
	BlockerLocation  Blocker = "location"
	BlockerBudget    Blocker = "budget"
	BlockerScan      Blocker = "scan"
	BlockerIntegrity Blocker = "integrity"
	BlockerRetrieval Blocker = "retrieval"
	BlockerCanceled  Blocker = "canceled"
)

type EvaluationStatus string

const (
	StatusEvaluated    EvaluationStatus = "evaluated"
	StatusNotEvaluated EvaluationStatus = "not-evaluated"
)

type QueryCategory string

const (
	CategoryExactSymbol       QueryCategory = "exact_symbol"
	CategoryConcept           QueryCategory = "concept"
	CategoryCrossLayer        QueryCategory = "cross_layer"
	CategoryFramework         QueryCategory = "framework"
	CategoryErrorMessage      QueryCategory = "error_message"
	CategoryConfigurationPath QueryCategory = "configuration_path"
	CategoryNegativeEvidence  QueryCategory = "negative_evidence"
)

type SymbolKind string

const (
	SymbolFunction  SymbolKind = "function"
	SymbolClass     SymbolKind = "class"
	SymbolInterface SymbolKind = "interface"
	SymbolType      SymbolKind = "type"
	SymbolEnum      SymbolKind = "enum"
	SymbolOther     SymbolKind = "other"
)

type Extension string

type SizeBand string

const (
	SizeBand0To4KiB      SizeBand = "0-4KiB"
	SizeBand4To16KiB     SizeBand = "4-16KiB"
	SizeBand16To64KiB    SizeBand = "16-64KiB"
	SizeBand64To256KiB   SizeBand = "64-256KiB"
	SizeBand256KiBTo1MiB SizeBand = "256KiB-1MiB"
)

type PatternBucket string

const (
	PatternTest          PatternBucket = "test"
	PatternMigration     PatternBucket = "migration"
	PatternConfiguration PatternBucket = "permitted_configuration"
)

type FallbackReason string

const (
	FallbackZero            FallbackReason = "zero"
	FallbackLexicalBaseline FallbackReason = "lexical_baseline"
)

type Limitation string

const (
	LimitationHeapApproximate  Limitation = "heap_peak_approximate"
	LimitationProcessLatency   Limitation = "process_local_latency"
	LimitationSyntheticSymbols Limitation = "synthetic_exact_symbol_only"
	LimitationFrameworkUnknown Limitation = "frameworks_not_inferred"
)

type ExtensionAggregate struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

type InventoryReport struct {
	EligibleFiles         int                              `json:"eligible_files"`
	EligibleBytes         int64                            `json:"eligible_bytes"`
	IncludedFiles         int                              `json:"included_files"`
	IncludedBytes         int64                            `json:"included_bytes"`
	ExcludedByCategory    map[ingest.ExclusionReason]int   `json:"excluded_by_category"`
	SupportedLanguages    map[source.Language]int          `json:"supported_languages"`
	SupportedExtensions   map[Extension]int                `json:"supported_extensions"`
	UnsupportedExtensions map[Extension]ExtensionAggregate `json:"unsupported_extensions"`
	SizeBands             map[SizeBand]int                 `json:"size_bands"`
	SymbolKinds           map[SymbolKind]int               `json:"symbol_kinds"`
	FilesWithoutSymbols   int                              `json:"files_without_symbols"`
	PatternBuckets        map[PatternBucket]int            `json:"pattern_buckets"`
	Chunks                int                              `json:"chunks"`
	IndexedBytes          int64                            `json:"indexed_bytes"`
}

type CategoryMetrics struct {
	Status     EvaluationStatus `json:"status"`
	QueryCount int              `json:"query_count"`
	RecallAt5  float64          `json:"recall_at_5"`
	RecallAt10 float64          `json:"recall_at_10"`
	MRRAt10    float64          `json:"mrr_at_10"`
	NDCGAt10   float64          `json:"ndcg_at_10"`
}

type RetrievalReport struct {
	Status                 EvaluationStatus                  `json:"status"`
	Categories             map[QueryCategory]CategoryMetrics `json:"categories"`
	CitationValidity       float64                           `json:"citation_validity"`
	DeterministicOrderRate float64                           `json:"deterministic_order_rate"`
	FallbackCount          int                               `json:"fallback_count"`
	FallbackReason         FallbackReason                    `json:"fallback_reason"`
}

type PerformanceReport struct {
	ScanMilliseconds  int64  `json:"scan_milliseconds"`
	IndexMilliseconds int64  `json:"index_milliseconds"`
	QueryP50Micros    int64  `json:"query_p50_microseconds"`
	QueryP95Micros    int64  `json:"query_p95_microseconds"`
	PeakHeapBytes     uint64 `json:"peak_heap_bytes_approximate"`
}

type Report struct {
	Schema         int                                `json:"schema"`
	Repository     string                             `json:"repository"`
	Decision       Decision                           `json:"decision"`
	Blockers       map[Blocker]int                    `json:"blockers"`
	Inventory      InventoryReport                    `json:"inventory"`
	Retrieval      RetrievalReport                    `json:"retrieval"`
	Performance    PerformanceReport                  `json:"performance"`
	CapabilityGaps map[QueryCategory]EvaluationStatus `json:"capability_gaps"`
	Limitations    map[Limitation]int                 `json:"limitations"`
}

// EmptyReport returns a complete schema with no private or query-level fields.
func EmptyReport(repository string, decision Decision) Report {
	return Report{
		Schema:     SchemaVersion,
		Repository: repository,
		Decision:   decision,
		Blockers:   make(map[Blocker]int),
		Inventory: InventoryReport{
			ExcludedByCategory:    make(map[ingest.ExclusionReason]int),
			SupportedLanguages:    make(map[source.Language]int),
			SupportedExtensions:   make(map[Extension]int),
			UnsupportedExtensions: make(map[Extension]ExtensionAggregate),
			SizeBands:             make(map[SizeBand]int),
			SymbolKinds:           make(map[SymbolKind]int),
			PatternBuckets:        make(map[PatternBucket]int),
		},
		Retrieval: RetrievalReport{
			Status: StatusNotEvaluated,
			Categories: map[QueryCategory]CategoryMetrics{
				CategoryExactSymbol:       {Status: StatusNotEvaluated},
				CategoryConcept:           {Status: StatusNotEvaluated},
				CategoryCrossLayer:        {Status: StatusNotEvaluated},
				CategoryFramework:         {Status: StatusNotEvaluated},
				CategoryErrorMessage:      {Status: StatusNotEvaluated},
				CategoryConfigurationPath: {Status: StatusNotEvaluated},
				CategoryNegativeEvidence:  {Status: StatusNotEvaluated},
			},
			FallbackReason: FallbackZero,
		},
		CapabilityGaps: map[QueryCategory]EvaluationStatus{
			CategoryConcept:           StatusNotEvaluated,
			CategoryCrossLayer:        StatusNotEvaluated,
			CategoryFramework:         StatusNotEvaluated,
			CategoryErrorMessage:      StatusNotEvaluated,
			CategoryConfigurationPath: StatusNotEvaluated,
			CategoryNegativeEvidence:  StatusNotEvaluated,
		},
		Limitations: map[Limitation]int{
			LimitationHeapApproximate:  1,
			LimitationProcessLatency:   1,
			LimitationSyntheticSymbols: 1,
			LimitationFrameworkUnknown: 1,
		},
	}
}
