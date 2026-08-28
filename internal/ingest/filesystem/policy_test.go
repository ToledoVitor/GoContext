package filesystem_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/ToledoVitor/GoContext/internal/ingest"
	"github.com/ToledoVitor/GoContext/internal/ingest/filesystem"
)

func TestScannerHardDenyTableIsCaseInsensitiveAndAudited(t *testing.T) {
	root := t.TempDir()
	securityDirectories := []string{
		".git", ".github", ".gitlab", ".circleci", ".azure", ".aws", ".ssh", ".gnupg", ".kube",
		".gocontext", ".idea", ".vscode", ".devcontainer", ".terraform", ".serverless",
	}
	builtInDirectories := []string{
		"node_modules", "vendor", ".venv", "venv", "__pycache__", ".pytest_cache", ".mypy_cache",
		".ruff_cache", ".cache", ".next", ".nuxt", ".svelte-kit", "dist", "build", "out", "target",
		"coverage", "tmp", "temp", "Pods", ".gradle", ".dart_tool", ".pub-cache", "DerivedData",
		"Carthage", ".cxx", ".expo", ".turbo", ".nx", ".parcel-cache", ".vite", ".bundle",
	}
	for _, directory := range append(append([]string(nil), securityDirectories...), builtInDirectories...) {
		writeFile(t, root, filepath.Join(directory, "allowed.ts"), "export const visible = true\n")
	}
	writeFile(t, root, filepath.Join("nested", ".GiThUb", "allowed.py"), "VISIBLE = true\n")
	writeFile(t, root, filepath.Join("nested", ".DaRt_ToOl", "allowed.py"), "VISIBLE = true\n")
	writeFile(t, root, filepath.Join("packages", "allowed.ts"), "export const visible = true\n")

	securityBasenames := []string{
		".env", ".env.local", ".npmrc", ".pypirc", ".netrc", ".htpasswd",
		"credentials", "credentials.py", "secret", "secret.ts", "secrets", "secrets.prod.ts",
		"id_rsa", "id_rsa_backup", "id_ed25519", "id_ed25519.pub", "authorized_keys", "known_hosts",
		"Jenkinsfile", ".travis.yml", "azure-pipelines.yml", "bitbucket-pipelines.yml", "renovate.json", "dependabot.yml",
	}
	for _, basename := range securityBasenames {
		writeFile(t, root, filepath.Join("names", basename), "not opened\n")
	}
	for _, basename := range []string{"CrEdEnTiAlS.PY", ".EnV.TS"} {
		writeFile(t, root, filepath.Join("case-variants", basename), "not opened\n")
	}
	securityBasenames = append(securityBasenames, "CrEdEnTiAlS.PY", ".EnV.TS")

	securitySuffixes := []string{
		"certificate.pem", "private.key", "identity.p12", "identity.pfx", "certificate.crt", "certificate.cer",
		"certificate.der", "identity.jks", "identity.keystore", "vault.kdbx", "signed.asc", "encrypted.gpg",
		"state.tfstate", "state.tfstate.backup",
	}
	for _, basename := range securitySuffixes {
		writeFile(t, root, filepath.Join("suffixes", basename), "not opened\n")
	}

	generatedSuffixes := []string{"bundle.min.ts", "bundle.map", "client.generated.ts", "client.gen.py"}
	for _, basename := range generatedSuffixes {
		writeFile(t, root, filepath.Join("suffixes", basename), "not opened\n")
	}
	writeFile(t, root, filepath.Join("case-variants", "BUNDLE.MIN.TS"), "not opened\n")
	generatedSuffixes = append(generatedSuffixes, "BUNDLE.MIN.TS")

	result, err := filesystem.NewScanner().Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(result.Files) != 1 || result.Files[0].Reference.Path != "packages/allowed.ts" {
		t.Fatalf("Scan() files = %#v, want only ambiguous packages directory content", result.Files)
	}
	if got, want := result.Report.Excluded[ingest.ExclusionSecurity], len(securityDirectories)+1+len(securityBasenames)+len(securitySuffixes); got != want {
		t.Errorf("security exclusions = %d, want %d", got, want)
	}
	if got, want := result.Report.Excluded[ingest.ExclusionDependencyBuildCache], len(builtInDirectories)+1; got != want {
		t.Errorf("dependency/build/cache exclusions = %d, want %d", got, want)
	}
	if got, want := result.Report.Excluded[ingest.ExclusionGenerated], len(generatedSuffixes); got != want {
		t.Errorf("generated exclusions = %d, want %d", got, want)
	}
	if len(result.Report.UnsupportedByExtension) != 0 {
		t.Errorf("UnsupportedByExtension = %#v, want empty because hard deny precedes allowlist", result.Report.UnsupportedByExtension)
	}
}

func TestScannerExpandedUnsupportedTaxonomyRemainsReportOnly(t *testing.T) {
	root := t.TempDir()
	extensions := []string{
		".dart", ".m", ".mm", ".gradle", ".properties", ".lock", ".podspec", ".xcconfig", ".pbxproj",
		".plist", ".storyboard", ".xib", ".graphql", ".gql", ".proto", ".svelte", ".astro", ".pyi",
		".svg", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".bmp", ".ico", ".tif", ".tiff",
		".ttf", ".otf", ".woff", ".woff2", ".eot",
		".zip", ".tar", ".gz", ".tgz", ".bz2", ".xz", ".7z", ".rar", ".jar", ".war", ".aar", ".apk", ".ipa",
		".so", ".dylib", ".dll", ".a", ".lib", ".o", ".obj", ".exe", ".bin", ".wasm",
	}
	for index, extension := range extensions {
		writeFile(t, root, fmt.Sprintf("artifact-%02d%s", index, extension), "aggregate-only\n")
	}

	result, err := filesystem.NewScanner().Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(result.Files) != 0 {
		t.Fatalf("Scan() returned %d files, want no newly ingestible extensions", len(result.Files))
	}
	if got := result.Report.Excluded[ingest.ExclusionUnsupportedExtension]; got != len(extensions) {
		t.Fatalf("unsupported exclusions = %d, want %d", got, len(extensions))
	}
	for _, extension := range extensions {
		if got := result.Report.UnsupportedByExtension[extension]; got != 1 {
			t.Errorf("unsupported %s count = %d, want 1", extension, got)
		}
	}
}

func TestScannerDoesNotUseRepositoryGitignoreToChangePolicy(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitignore", "keep.py\n!.env.py\n")
	writeFile(t, root, "keep.py", "print('still included')\n")
	writeFile(t, root, ".env.py", "TOKEN = 'still denied'\n")

	result, err := filesystem.NewScanner().Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(result.Files) != 1 || result.Files[0].Reference.Path != "keep.py" {
		t.Fatalf("Scan() files = %#v, want only keep.py", result.Files)
	}
	if result.Report.Excluded[ingest.ExclusionSecurity] != 1 {
		t.Errorf("security exclusions = %d, want 1", result.Report.Excluded[ingest.ExclusionSecurity])
	}
}
