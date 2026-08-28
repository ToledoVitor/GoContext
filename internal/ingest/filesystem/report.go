package filesystem

import (
	"path"
	"strings"

	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/source"
)

var safeUnsupportedExtensions = map[string]struct{}{
	".bash": {}, ".c": {}, ".cc": {}, ".cpp": {}, ".cs": {}, ".css": {}, ".csv": {},
	".go": {}, ".h": {}, ".hpp": {}, ".html": {}, ".java": {}, ".js": {}, ".jsx": {},
	".json": {}, ".kt": {}, ".kts": {}, ".less": {}, ".log": {}, ".lua": {}, ".md": {},
	".php": {}, ".rb": {}, ".rs": {}, ".sass": {}, ".scala": {}, ".scss": {}, ".sh": {},
	".sql": {}, ".swift": {}, ".toml": {}, ".txt": {}, ".vue": {}, ".xml": {}, ".yaml": {},
	".yml": {}, ".zsh": {},
}

func newScanReport() ingest.ScanReport {
	return ingest.ScanReport{
		Excluded:               make(map[ingest.ExclusionReason]int),
		IncludedByLanguage:     make(map[source.Language]int),
		SizeBands:              make(map[string]int),
		UnsupportedByExtension: make(map[string]int),
	}
}

func addExclusion(report *ingest.ScanReport, reason ingest.ExclusionReason) {
	report.Excluded[reason]++
}

func addUnsupported(report *ingest.ScanReport, name string) {
	addExclusion(report, ingest.ExclusionUnsupportedExtension)
	extension := strings.ToLower(path.Ext(name))
	if extension == "" {
		extension = "<none>"
	} else if _, safe := safeUnsupportedExtensions[extension]; !safe {
		extension = "<other>"
	}
	report.UnsupportedByExtension[extension]++
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
