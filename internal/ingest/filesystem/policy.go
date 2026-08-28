package filesystem

import (
	"bytes"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ToledoVitor/GoContext/internal/ingest"
)

var securityDirectories = []string{
	".git", ".github", ".gitlab", ".circleci", ".azure", ".aws", ".ssh", ".gnupg", ".kube",
	".gocontext", ".idea", ".vscode", ".devcontainer", ".terraform", ".serverless",
}

var dependencyBuildCacheDirectories = []string{
	"node_modules", "vendor", ".venv", "venv", "__pycache__", ".pytest_cache", ".mypy_cache",
	".ruff_cache", ".cache", ".next", ".nuxt", ".svelte-kit", "dist", "build", "out", "target",
	"coverage", "tmp", "temp",
}

var securityBasenames = []string{
	".git", ".npmrc", ".pypirc", ".netrc", ".htpasswd", "authorized_keys", "known_hosts", "jenkinsfile",
	".travis.yml", "azure-pipelines.yml", "bitbucket-pipelines.yml", "renovate.json", "dependabot.yml",
}

var securitySuffixes = []string{
	".pem", ".key", ".p12", ".pfx", ".crt", ".cer", ".der", ".jks", ".keystore",
	".kdbx", ".asc", ".gpg",
}

var knownCredentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	regexp.MustCompile(`\bgh[pousr]_[0-9A-Za-z]{20,}\b`),
	regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z-]{10,}\b`),
}

func classifyPath(name string, directory bool) (ingest.ExclusionReason, bool) {
	if directory {
		if equalFoldAny(name, securityDirectories) {
			return ingest.ExclusionSecurity, true
		}
		if equalFoldAny(name, dependencyBuildCacheDirectories) {
			return ingest.ExclusionDependencyBuildCache, true
		}
		return "", false
	}

	if equalFoldAny(name, securityBasenames) ||
		strings.EqualFold(name, ".env") || hasPrefixFold(name, ".env.") ||
		matchesNamedSecret(name, "credentials") ||
		matchesNamedSecret(name, "secret") ||
		matchesNamedSecret(name, "secrets") ||
		hasPrefixFold(name, "id_rsa") ||
		hasPrefixFold(name, "id_ed25519") {
		return ingest.ExclusionSecurity, true
	}
	if equalFoldAny(path.Ext(name), securitySuffixes) ||
		hasSuffixFold(name, ".tfstate") || containsFold(name, ".tfstate.") {
		return ingest.ExclusionSecurity, true
	}
	if containsFold(name, ".min.") || hasSuffixFold(name, ".map") ||
		containsFold(name, ".generated.") || containsFold(name, ".gen.") {
		return ingest.ExclusionGenerated, true
	}
	return "", false
}

func matchesNamedSecret(name, stem string) bool {
	return strings.EqualFold(name, stem) || hasPrefixFold(name, stem+".")
}

func equalFoldAny(value string, candidates []string) bool {
	for _, candidate := range candidates {
		if strings.EqualFold(value, candidate) {
			return true
		}
	}
	return false
}

func hasPrefixFold(value, prefix string) bool {
	return len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix)
}

func hasSuffixFold(value, suffix string) bool {
	return len(value) >= len(suffix) && strings.EqualFold(value[len(value)-len(suffix):], suffix)
}

func containsFold(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if strings.EqualFold(value[index:index+len(fragment)], fragment) {
			return true
		}
	}
	return false
}

// classifyContent is intentionally conservative and can reject false
// positives. An allowed filename must be read before an unknown embedded
// secret can be classified; rejected content never leaves the scanner.
func classifyContent(content []byte) (ingest.ExclusionReason, bool) {
	if !utf8.Valid(content) {
		return ingest.ExclusionInvalidUTF8, true
	}
	header := content
	if len(header) > 4<<10 {
		header = header[:4<<10]
	}
	lowerHeader := bytes.ToLower(header)
	if bytes.Contains(lowerHeader, []byte("code generated")) && bytes.Contains(lowerHeader, []byte("do not edit")) {
		return ingest.ExclusionGenerated, true
	}
	if containsSecret(content) {
		return ingest.ExclusionSecret, true
	}
	return "", false
}

func containsSecret(content []byte) bool {
	upperContent := bytes.ToUpper(content)
	for _, marker := range [][]byte{
		[]byte("-----BEGIN PRIVATE KEY-----"),
		[]byte("-----BEGIN RSA PRIVATE KEY-----"),
		[]byte("-----BEGIN EC PRIVATE KEY-----"),
		[]byte("-----BEGIN OPENSSH PRIVATE KEY-----"),
	} {
		if bytes.Contains(upperContent, marker) {
			return true
		}
	}
	for _, pattern := range knownCredentialPatterns {
		if pattern.Match(content) {
			return true
		}
	}
	for _, line := range strings.Split(string(content), "\n") {
		if containsLiteralSecretAssignment(line) {
			return true
		}
	}
	return false
}

func containsLiteralSecretAssignment(line string) bool {
	delimiter := strings.IndexAny(line, "=:")
	if delimiter < 0 {
		return false
	}
	left := strings.ToLower(line[:delimiter])
	if !strings.Contains(left, "token") && !strings.Contains(left, "password") &&
		!strings.Contains(left, "secret") && !strings.Contains(left, "api_key") && !strings.Contains(left, "apikey") {
		return false
	}
	right := strings.TrimSpace(line[delimiter+1:])
	if len(right) < 2 || (right[0] != '\'' && right[0] != '"' && right[0] != '`') {
		return false
	}
	return strings.ContainsRune(right[1:], rune(right[0]))
}
