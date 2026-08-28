package filesystem

import (
	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/source"
)

func newScanReport() ingest.ScanReport {
	return ingest.ScanReport{
		Excluded:                    make(map[ingest.ExclusionReason]int),
		IncludedByLanguage:          make(map[source.Language]int),
		SizeBands:                   make(map[string]int),
		UnsupportedByExtension:      make(map[string]int),
		UnsupportedBytesByExtension: make(map[string]int64),
	}
}

func addExclusion(report *ingest.ScanReport, reason ingest.ExclusionReason) {
	report.Excluded[reason]++
}

func addUnsupported(report *ingest.ScanReport, name string, bytes int64) {
	addExclusion(report, ingest.ExclusionUnsupportedExtension)
	extension := ingest.UnsupportedExtensionBucket(name)
	report.UnsupportedByExtension[extension]++
	report.UnsupportedBytesByExtension[extension] += bytes
}

func addIncluded(report *ingest.ScanReport, language source.Language, bytes int64) {
	report.IncludedFiles++
	report.IncludedBytes += bytes
	report.IncludedByLanguage[language]++
	report.SizeBands[sizeBand(bytes)]++
}

func sizeBand(bytes int64) string {
	switch {
	case bytes <= 4<<10:
		return "0-4KiB"
	case bytes <= 16<<10:
		return "4-16KiB"
	case bytes <= 64<<10:
		return "16-64KiB"
	case bytes <= 256<<10:
		return "64-256KiB"
	default:
		return "256KiB-1MiB"
	}
}
