package eval

import (
	"context"
	"errors"
	"path"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/search"
	"github.com/ToledoVitor/GoContext/internal/source"
)

var ErrEvaluation = errors.New("local evaluation failed")

type SearchFactory func(repositoryID string, corpus []source.Chunk) (search.Searcher, error)

type Dependencies struct {
	Scanner       ingest.Scanner
	Parser        ingest.Parser
	Chunker       ingest.Chunker
	SearchFactory SearchFactory
	Now           func() time.Time
	ReadHeapBytes func() uint64
}

type Budgets struct {
	MaxEligibleFiles int
	MaxEligibleBytes int64
	MaxAutoQueries   int
}

// Evaluate is the module's small external interface. It owns the single scan,
// parsing, chunking, corpus integrity validation, deterministic case generation,
// and aggregate metrics. Concrete adapters are supplied by the CLI composition
// root.
func Evaluate(
	ctx context.Context,
	repositoryID string,
	root string,
	dependencies Dependencies,
	budgets Budgets,
) (Report, error) {
	if err := ctx.Err(); err != nil {
		return failedReport(repositoryID, BlockerCanceled), err
	}
	if !opaqueRepositoryPattern.MatchString(repositoryID) || interfaceNil(dependencies.Scanner) ||
		interfaceNil(dependencies.Parser) || interfaceNil(dependencies.Chunker) || dependencies.SearchFactory == nil {
		return failedReport(repositoryID, BlockerIntegrity), ErrEvaluation
	}
	if budgets.MaxEligibleFiles <= 0 || budgets.MaxEligibleBytes <= 0 || budgets.MaxAutoQueries <= 0 {
		return failedReport(repositoryID, BlockerBudget), ErrEvaluation
	}
	now := dependencies.Now
	if now == nil {
		now = time.Now
	}
	readHeap := dependencies.ReadHeapBytes
	if readHeap == nil {
		readHeap = currentHeapBytes
	}
	peakHeap := readHeap()
	sampleHeap := func() {
		if current := readHeap(); current > peakHeap {
			peakHeap = current
		}
	}

	scanStarted := now()
	scanResult, err := dependencies.Scanner.Scan(ctx, root)
	scanFinished := now()
	if err != nil {
		if ctx.Err() != nil {
			return failedReport(repositoryID, BlockerCanceled), ctx.Err()
		}
		return failedReport(repositoryID, BlockerScan), ErrEvaluation
	}
	if scanFinished.Before(scanStarted) {
		return failedReport(repositoryID, BlockerIntegrity), ErrEvaluation
	}
	sampleHeap()
	if scanResult.Report.EligibleFiles > budgets.MaxEligibleFiles || scanResult.Report.EligibleBytes > budgets.MaxEligibleBytes {
		return failedReport(repositoryID, BlockerBudget), ErrEvaluation
	}
	if !validScanResult(scanResult) {
		return failedReport(repositoryID, BlockerIntegrity), ErrEvaluation
	}

	report := EmptyReport(repositoryID, DecisionGo)
	report.Performance.ScanMilliseconds = scanFinished.Sub(scanStarted).Milliseconds()
	report.Inventory = aggregateScanInventory(scanResult)
	indexStarted := now()
	chunks := make([]source.Chunk, 0, len(scanResult.Files))
	for _, file := range scanResult.Files {
		if err := ctx.Err(); err != nil {
			return failedReport(repositoryID, BlockerCanceled), err
		}
		symbols, parseErr := dependencies.Parser.Parse(ctx, file)
		if parseErr != nil || !validSymbolsForFile(file, symbols) {
			if ctx.Err() != nil {
				return failedReport(repositoryID, BlockerCanceled), ctx.Err()
			}
			return failedReport(repositoryID, BlockerIntegrity), ErrEvaluation
		}
		if len(symbols) == 0 {
			report.Inventory.FilesWithoutSymbols++
		}
		for _, symbol := range symbols {
			report.Inventory.SymbolKinds[allowlistedSymbolKind(symbol.Kind)]++
		}
		fileChunks, chunkErr := dependencies.Chunker.Chunk(ctx, file, symbols)
		if chunkErr != nil || !validChunksForFile(file, symbols, fileChunks) {
			if ctx.Err() != nil {
				return failedReport(repositoryID, BlockerCanceled), ctx.Err()
			}
			return failedReport(repositoryID, BlockerIntegrity), ErrEvaluation
		}
		for _, chunk := range fileChunks {
			report.Inventory.IndexedBytes += int64(len(chunk.Text))
		}
		chunks = append(chunks, fileChunks...)
	}
	corpus, err := source.NewCorpusContext(ctx, scanResult.PolicyVersion, chunks)
	if err != nil {
		if ctx.Err() != nil {
			return failedReport(repositoryID, BlockerCanceled), ctx.Err()
		}
		return failedReport(repositoryID, BlockerIntegrity), ErrEvaluation
	}
	report.Inventory.Chunks = len(corpus.Chunks)
	indexFinished := now()
	if indexFinished.Before(indexStarted) {
		return failedReport(repositoryID, BlockerIntegrity), ErrEvaluation
	}
	report.Performance.IndexMilliseconds = indexFinished.Sub(indexStarted).Milliseconds()
	sampleHeap()

	cases := exactSymbolCases(corpus.Chunks, budgets.MaxAutoQueries)
	if len(cases) > 0 {
		searcher, factoryErr := dependencies.SearchFactory(repositoryID, append([]source.Chunk(nil), corpus.Chunks...))
		if factoryErr != nil || interfaceNil(searcher) {
			return failedReport(repositoryID, BlockerRetrieval), ErrEvaluation
		}
		metrics, metricErr := EvaluateMetrics(ctx, searcher, repositoryID, corpus.Chunks, cases, MetricOptions{Now: now})
		if metricErr != nil {
			if ctx.Err() != nil {
				return failedReport(repositoryID, BlockerCanceled), ctx.Err()
			}
			return failedReport(repositoryID, BlockerRetrieval), ErrEvaluation
		}
		report.Retrieval = metrics.Report
		report.Performance.QueryP50Micros = metrics.QueryP50Micros
		report.Performance.QueryP95Micros = metrics.QueryP95Micros
	}
	sampleHeap()
	report.Performance.PeakHeapBytes = peakHeap
	if _, err := MarshalValidated(report); err != nil {
		return failedReport(repositoryID, BlockerIntegrity), ErrEvaluation
	}
	return report, nil
}

func failedReport(repositoryID string, blocker Blocker) Report {
	if !opaqueRepositoryPattern.MatchString(repositoryID) {
		repositoryID = "repo-00"
	}
	report := EmptyReport(repositoryID, DecisionNoGo)
	report.Blockers[blocker] = 1
	return report
}

func validScanResult(result ingest.ScanResult) bool {
	if strings.TrimSpace(result.PolicyVersion) == "" || result.Report.EligibleFiles < 0 || result.Report.EligibleBytes < 0 ||
		result.Report.IncludedFiles != len(result.Files) || result.Report.IncludedFiles < 0 || result.Report.IncludedBytes < 0 ||
		result.Report.IncludedFiles > result.Report.EligibleFiles || result.Report.IncludedBytes > result.Report.EligibleBytes {
		return false
	}
	includedBytes := int64(0)
	languages := make(map[source.Language]int)
	for _, file := range result.Files {
		if !file.Reference.Valid() || (file.Language != source.LanguagePython && file.Language != source.LanguageTypeScript) {
			return false
		}
		includedBytes += int64(len(file.Content))
		languages[file.Language]++
	}
	if includedBytes != result.Report.IncludedBytes || !equalLanguageCounts(languages, result.Report.IncludedByLanguage) {
		return false
	}
	for reason, count := range result.Report.Excluded {
		if count < 0 || !allowedExclusionReason(reason) {
			return false
		}
	}
	unsupportedFiles := 0
	for extension, count := range result.Report.UnsupportedByExtension {
		if count < 0 || !allowedUnsupportedExtension(Extension(extension)) || result.Report.UnsupportedBytesByExtension[extension] < 0 {
			return false
		}
		unsupportedFiles += count
	}
	for extension, bytes := range result.Report.UnsupportedBytesByExtension {
		if bytes < 0 || !allowedUnsupportedExtension(Extension(extension)) {
			return false
		}
	}
	return unsupportedFiles == result.Report.Excluded[ingest.ExclusionUnsupportedExtension]
}

func aggregateScanInventory(result ingest.ScanResult) InventoryReport {
	inventory := EmptyReport("repo-00", DecisionGo).Inventory
	inventory.EligibleFiles = result.Report.EligibleFiles
	inventory.EligibleBytes = result.Report.EligibleBytes
	inventory.IncludedFiles = result.Report.IncludedFiles
	inventory.IncludedBytes = result.Report.IncludedBytes
	for reason, count := range result.Report.Excluded {
		inventory.ExcludedByCategory[reason] = count
	}
	for language, count := range result.Report.IncludedByLanguage {
		inventory.SupportedLanguages[language] = count
	}
	for band, count := range result.Report.SizeBands {
		inventory.SizeBands[SizeBand(band)] = count
	}
	for extension, count := range result.Report.UnsupportedByExtension {
		inventory.UnsupportedExtensions[Extension(extension)] = ExtensionAggregate{
			Files: count,
			Bytes: result.Report.UnsupportedBytesByExtension[extension],
		}
	}
	for _, file := range result.Files {
		inventory.SupportedExtensions[Extension(strings.ToLower(path.Ext(file.Reference.Path)))]++
		for _, bucket := range pathPatternBuckets(file.Reference.Path) {
			inventory.PatternBuckets[bucket]++
		}
	}
	return inventory
}

func validSymbolsForFile(file source.File, symbols []source.Symbol) bool {
	previousLine := file.Reference.StartLine - 1
	for _, symbol := range symbols {
		if strings.TrimSpace(symbol.Name) == "" || !symbol.Reference.Valid() || symbol.Reference.Path != file.Reference.Path ||
			symbol.Reference.StartLine <= previousLine || symbol.Reference.StartLine < file.Reference.StartLine ||
			symbol.Reference.EndLine > file.Reference.EndLine {
			return false
		}
		previousLine = symbol.Reference.StartLine
	}
	return true
}

func validChunksForFile(file source.File, symbols []source.Symbol, chunks []source.Chunk) bool {
	if (len(symbols) > 0 && len(chunks) != len(symbols)) || (len(symbols) == 0 && len(chunks) > 1) {
		return false
	}
	symbolNames := make(map[string]struct{}, len(symbols))
	for _, symbol := range symbols {
		symbolNames[symbol.Name] = struct{}{}
	}
	for index, chunk := range chunks {
		if chunk.ID == "" || strings.TrimSpace(chunk.Text) == "" || !chunk.Reference.Valid() ||
			chunk.Reference.Path != file.Reference.Path || chunk.Reference.StartLine < file.Reference.StartLine ||
			chunk.Reference.EndLine > file.Reference.EndLine || chunk.Language != file.Language {
			return false
		}
		if chunk.SymbolName != "" {
			if _, present := symbolNames[chunk.SymbolName]; !present {
				return false
			}
			if index >= len(symbols) || chunk.SymbolName != symbols[index].Name ||
				chunk.Reference.StartLine > symbols[index].Reference.StartLine || chunk.Reference.EndLine < symbols[index].Reference.EndLine {
				return false
			}
		} else if len(symbols) > 0 {
			return false
		}
	}
	return true
}

func exactSymbolCases(chunks []source.Chunk, limit int) []Case {
	sortedChunks := append([]source.Chunk(nil), chunks...)
	sort.Slice(sortedChunks, func(left, right int) bool { return sortedChunks[left].ID < sortedChunks[right].ID })
	idsByName := make(map[string][]string)
	orderedNames := make([]string, 0)
	for _, chunk := range sortedChunks {
		if strings.TrimSpace(chunk.SymbolName) == "" {
			continue
		}
		if _, present := idsByName[chunk.SymbolName]; !present {
			orderedNames = append(orderedNames, chunk.SymbolName)
		}
		idsByName[chunk.SymbolName] = append(idsByName[chunk.SymbolName], chunk.ID)
	}
	if len(orderedNames) > limit {
		orderedNames = orderedNames[:limit]
	}
	cases := make([]Case, 0, len(orderedNames))
	for _, name := range orderedNames {
		cases = append(cases, Case{
			Category: CategoryExactSymbol, Query: name,
			RelevantChunkIDs: append([]string(nil), idsByName[name]...),
		})
	}
	return cases
}

// pathPatternBuckets intentionally uses only generic, allowlisted rules on
// already-permitted paths: conventional test segments/stems, migration
// segments, and a literal config/configuration stem. It never infers a
// framework, route, model, or other content-specific category.
func pathPatternBuckets(filePath string) []PatternBucket {
	segments := strings.Split(strings.ToLower(filePath), "/")
	stem := strings.TrimSuffix(segments[len(segments)-1], path.Ext(segments[len(segments)-1]))
	found := make(map[PatternBucket]bool)
	for _, segment := range segments[:len(segments)-1] {
		switch segment {
		case "test", "tests", "__tests__":
			found[PatternTest] = true
		case "migration", "migrations":
			found[PatternMigration] = true
		}
	}
	if strings.HasPrefix(stem, "test_") || strings.HasSuffix(stem, "_test") || strings.HasSuffix(stem, ".test") || strings.HasSuffix(stem, ".spec") {
		found[PatternTest] = true
	}
	if stem == "config" || stem == "configuration" || strings.HasSuffix(stem, ".config") {
		found[PatternConfiguration] = true
	}
	ordered := []PatternBucket{PatternTest, PatternMigration, PatternConfiguration}
	buckets := make([]PatternBucket, 0, len(found))
	for _, bucket := range ordered {
		if found[bucket] {
			buckets = append(buckets, bucket)
		}
	}
	return buckets
}

func allowlistedSymbolKind(kind string) SymbolKind {
	switch SymbolKind(kind) {
	case SymbolFunction, SymbolClass, SymbolInterface, SymbolType, SymbolEnum:
		return SymbolKind(kind)
	default:
		return SymbolOther
	}
}

func allowedExclusionReason(reason ingest.ExclusionReason) bool {
	switch reason {
	case ingest.ExclusionSecurity, ingest.ExclusionDependencyBuildCache, ingest.ExclusionNestedRepository,
		ingest.ExclusionSymlink, ingest.ExclusionUnsupportedExtension, ingest.ExclusionNonRegular,
		ingest.ExclusionTooLarge, ingest.ExclusionBinary, ingest.ExclusionInvalidUTF8,
		ingest.ExclusionGenerated, ingest.ExclusionSecret:
		return true
	default:
		return false
	}
}

func equalLanguageCounts(left, right map[source.Language]int) bool {
	if len(left) != len(right) {
		return false
	}
	for language, count := range left {
		if right[language] != count || count < 0 {
			return false
		}
	}
	return true
}

func currentHeapBytes() uint64 {
	var statistics runtime.MemStats
	runtime.ReadMemStats(&statistics)
	return statistics.HeapAlloc
}
