package ingest

import (
	"path"
	"strings"
)

const (
	UnsupportedExtensionOther = "<other>"
	UnsupportedExtensionNone  = "<none>"
)

var safeUnsupportedExtensions = map[string]struct{}{
	".bash": {}, ".c": {}, ".cc": {}, ".cpp": {}, ".cs": {}, ".css": {}, ".csv": {},
	".go": {}, ".h": {}, ".hpp": {}, ".html": {}, ".java": {}, ".js": {}, ".jsx": {},
	".json": {}, ".kt": {}, ".kts": {}, ".less": {}, ".log": {}, ".lua": {}, ".md": {},
	".php": {}, ".rb": {}, ".rs": {}, ".sass": {}, ".scala": {}, ".scss": {}, ".sh": {},
	".sql": {}, ".swift": {}, ".toml": {}, ".txt": {}, ".vue": {}, ".xml": {}, ".yaml": {},
	".yml": {}, ".zsh": {},
}

// UnsupportedExtensionBucket returns an aggregate-only extension label that
// cannot disclose an arbitrary filename or uncommon extension.
func UnsupportedExtensionBucket(name string) string {
	extension := strings.ToLower(path.Ext(name))
	if extension == "" {
		return UnsupportedExtensionNone
	}
	if _, safe := safeUnsupportedExtensions[extension]; !safe {
		return UnsupportedExtensionOther
	}
	return extension
}

// SafeUnsupportedExtensionBucket reports whether a value is a canonical,
// sanitized unsupported-extension aggregate label.
func SafeUnsupportedExtensionBucket(bucket string) bool {
	if bucket == UnsupportedExtensionOther || bucket == UnsupportedExtensionNone {
		return true
	}
	_, safe := safeUnsupportedExtensions[bucket]
	return safe
}
