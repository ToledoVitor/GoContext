package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"

	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/source"
)

var (
	errInvalidReport        = errors.New("invalid evaluation report")
	opaqueRepositoryPattern = regexp.MustCompile(`^repo-[a-f0-9]{2,64}$`)
)

func MarshalValidated(report Report) ([]byte, error) {
	if !validReport(report) {
		return nil, errInvalidReport
	}
	payload, err := json.Marshal(report)
	if err != nil {
		return nil, errInvalidReport
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var decoded Report
	if err := decoder.Decode(&decoded); err != nil || !decoderAtEOF(decoder) || !validReport(decoded) {
		return nil, errInvalidReport
	}
	return append(payload, '\n'), nil
}

func decoderAtEOF(decoder *json.Decoder) bool {
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}

func validReport(report Report) bool {
	if report.Schema != SchemaVersion || !opaqueRepositoryPattern.MatchString(report.Repository) ||
		(report.Decision != DecisionGo && report.Decision != DecisionNoGo) || !validBlockers(report.Blockers) ||
		!validInventory(report.Inventory) || !validRetrieval(report.Retrieval) ||
		report.Performance.ScanMilliseconds < 0 || report.Performance.IndexMilliseconds < 0 ||
		report.Performance.QueryP50Micros < 0 || report.Performance.QueryP95Micros < 0 ||
		report.Performance.QueryP50Micros > report.Performance.QueryP95Micros ||
		!validCapabilityGaps(report.CapabilityGaps) || !validLimitations(report.Limitations) {
		return false
	}
	blockerCount := 0
	for _, count := range report.Blockers {
		blockerCount += count
	}
	return report.Decision == DecisionGo && blockerCount == 0 || report.Decision == DecisionNoGo && blockerCount > 0
}

func validBlockers(values map[Blocker]int) bool {
	allowed := map[Blocker]struct{}{
		BlockerChecklist: {}, BlockerRoot: {}, BlockerLocation: {}, BlockerBudget: {},
		BlockerScan: {}, BlockerIntegrity: {}, BlockerRetrieval: {}, BlockerCanceled: {},
	}
	return validCountMap(values, allowed)
}

func validInventory(value InventoryReport) bool {
	if value.EligibleFiles < 0 || value.EligibleBytes < 0 || value.IncludedFiles < 0 ||
		value.IncludedBytes < 0 || value.IncludedFiles > value.EligibleFiles || value.IncludedBytes > value.EligibleBytes ||
		value.FilesWithoutSymbols < 0 || value.FilesWithoutSymbols > value.IncludedFiles || value.Chunks < 0 || value.IndexedBytes < 0 {
		return false
	}
	exclusions := map[ingest.ExclusionReason]struct{}{
		ingest.ExclusionSecurity: {}, ingest.ExclusionDependencyBuildCache: {}, ingest.ExclusionNestedRepository: {},
		ingest.ExclusionSymlink: {}, ingest.ExclusionUnsupportedExtension: {}, ingest.ExclusionNonRegular: {},
		ingest.ExclusionTooLarge: {}, ingest.ExclusionBinary: {}, ingest.ExclusionInvalidUTF8: {},
		ingest.ExclusionGenerated: {}, ingest.ExclusionSecret: {},
	}
	languages := map[source.Language]struct{}{source.LanguagePython: {}, source.LanguageTypeScript: {}}
	supportedExtensions := map[Extension]struct{}{Extension(".py"): {}, Extension(".ts"): {}, Extension(".tsx"): {}}
	sizeBands := map[SizeBand]struct{}{
		SizeBand0To4KiB: {}, SizeBand4To16KiB: {}, SizeBand16To64KiB: {},
		SizeBand64To256KiB: {}, SizeBand256KiBTo1MiB: {},
	}
	symbolKinds := map[SymbolKind]struct{}{
		SymbolFunction: {}, SymbolClass: {}, SymbolInterface: {}, SymbolType: {}, SymbolEnum: {}, SymbolOther: {},
	}
	patterns := map[PatternBucket]struct{}{PatternTest: {}, PatternMigration: {}, PatternConfiguration: {}}
	if !validCountMap(value.ExcludedByCategory, exclusions) || !validCountMap(value.SupportedLanguages, languages) ||
		!validCountMap(value.SupportedExtensions, supportedExtensions) || !validCountMap(value.SizeBands, sizeBands) ||
		!validCountMap(value.SymbolKinds, symbolKinds) || !validCountMap(value.PatternBuckets, patterns) {
		return false
	}
	if sumCounts(value.SupportedLanguages) != value.IncludedFiles || sumCounts(value.SupportedExtensions) != value.IncludedFiles ||
		sumCounts(value.SizeBands) != value.IncludedFiles {
		return false
	}
	unsupportedFiles := 0
	for extension, aggregate := range value.UnsupportedExtensions {
		if !allowedUnsupportedExtension(extension) || aggregate.Files < 0 || aggregate.Bytes < 0 {
			return false
		}
		unsupportedFiles += aggregate.Files
	}
	return unsupportedFiles == value.ExcludedByCategory[ingest.ExclusionUnsupportedExtension]
}

func allowedUnsupportedExtension(extension Extension) bool {
	return ingest.SafeUnsupportedExtensionBucket(string(extension))
}

func validRetrieval(value RetrievalReport) bool {
	if value.Status != StatusEvaluated && value.Status != StatusNotEvaluated || value.FallbackCount < 0 ||
		value.FallbackReason != FallbackZero && value.FallbackReason != FallbackLexicalBaseline ||
		!validRatio(value.CitationValidity) || !validRatio(value.DeterministicOrderRate) {
		return false
	}
	allowed := map[QueryCategory]struct{}{
		CategoryExactSymbol: {}, CategoryConcept: {}, CategoryCrossLayer: {}, CategoryFramework: {},
		CategoryErrorMessage: {}, CategoryConfigurationPath: {}, CategoryNegativeEvidence: {},
	}
	evaluatedCategories := 0
	for category, metrics := range value.Categories {
		if _, ok := allowed[category]; !ok || !validCategoryMetrics(metrics) {
			return false
		}
		if metrics.Status == StatusEvaluated {
			evaluatedCategories++
		}
	}
	if len(value.Categories) != len(allowed) {
		return false
	}
	if value.Status == StatusNotEvaluated {
		return evaluatedCategories == 0 && value.CitationValidity == 0 && value.DeterministicOrderRate == 0 &&
			value.FallbackCount == 0 && value.FallbackReason == FallbackZero
	}
	return evaluatedCategories > 0 && value.FallbackReason == FallbackLexicalBaseline
}

func validCategoryMetrics(value CategoryMetrics) bool {
	if (value.Status != StatusEvaluated && value.Status != StatusNotEvaluated) || value.QueryCount < 0 ||
		!validRatio(value.RecallAt5) || !validRatio(value.RecallAt10) || !validRatio(value.MRRAt10) || !validRatio(value.NDCGAt10) {
		return false
	}
	if value.Status == StatusNotEvaluated {
		return value.QueryCount == 0 && value.RecallAt5 == 0 && value.RecallAt10 == 0 && value.MRRAt10 == 0 && value.NDCGAt10 == 0
	}
	return value.QueryCount > 0
}

func validCapabilityGaps(values map[QueryCategory]EvaluationStatus) bool {
	allowed := map[QueryCategory]struct{}{
		CategoryConcept: {}, CategoryCrossLayer: {}, CategoryFramework: {}, CategoryErrorMessage: {},
		CategoryConfigurationPath: {}, CategoryNegativeEvidence: {},
	}
	if len(values) != len(allowed) {
		return false
	}
	for category, status := range values {
		if _, ok := allowed[category]; !ok || status != StatusNotEvaluated {
			return false
		}
	}
	return true
}

func validLimitations(values map[Limitation]int) bool {
	allowed := map[Limitation]struct{}{
		LimitationHeapApproximate: {}, LimitationProcessLatency: {}, LimitationSyntheticSymbols: {}, LimitationFrameworkUnknown: {},
	}
	return validCountMap(values, allowed)
}

func validRatio(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func validCountMap[K comparable](values map[K]int, allowed map[K]struct{}) bool {
	if values == nil {
		return false
	}
	for key, count := range values {
		if _, ok := allowed[key]; !ok || count < 0 {
			return false
		}
	}
	return true
}

func sumCounts[K comparable](values map[K]int) int {
	total := 0
	for _, count := range values {
		total += count
	}
	return total
}
