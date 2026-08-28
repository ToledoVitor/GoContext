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

	".dart": {}, ".m": {}, ".mm": {}, ".gradle": {}, ".properties": {}, ".lock": {},
	".podspec": {}, ".xcconfig": {}, ".pbxproj": {}, ".plist": {}, ".storyboard": {}, ".xib": {},
	".graphql": {}, ".gql": {}, ".proto": {}, ".svelte": {}, ".astro": {}, ".pyi": {},

	".svg": {}, ".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {}, ".webp": {}, ".avif": {},
	".bmp": {}, ".ico": {}, ".tif": {}, ".tiff": {},
	".ttf": {}, ".otf": {}, ".woff": {}, ".woff2": {}, ".eot": {},
	".zip": {}, ".tar": {}, ".gz": {}, ".tgz": {}, ".bz2": {}, ".xz": {}, ".7z": {},
	".rar": {}, ".jar": {}, ".war": {}, ".aar": {}, ".apk": {}, ".ipa": {},
	".so": {}, ".dylib": {}, ".dll": {}, ".a": {}, ".lib": {}, ".o": {}, ".obj": {},
	".exe": {}, ".bin": {}, ".wasm": {},
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
