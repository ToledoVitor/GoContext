package index

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"strings"
)

const generationManifestDigestVersion = "generation-manifest-v1"

// GenerationManifest is the provider-neutral semantic identity persisted for
// one complete generation. VectorDigest is calculated by the concrete store
// over its canonical vector encoding.
type GenerationManifest struct {
	RepositoryID       string
	GenerationID       string
	CorpusRevision     string
	ContentDigest      string
	ScanPolicyVersion  string
	ProfileFingerprint string
	ProfileModel       string
	Dimensions         int
	Metric             VectorMetric
	VectorDigest       string
}

// GenerationManifestDigest returns the deterministic semantic identity for a
// complete generation without depending on an embedding provider or store.
func GenerationManifestDigest(manifest GenerationManifest) (string, error) {
	semanticState, err := validateGenerationManifest(manifest)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	for _, value := range [...]string{
		generationManifestDigestVersion,
		manifest.RepositoryID,
		manifest.GenerationID,
		manifest.CorpusRevision,
		manifest.ContentDigest,
		manifest.ScanPolicyVersion,
		semanticState,
		manifest.ProfileFingerprint,
		manifest.ProfileModel,
		string(manifest.Metric),
		manifest.VectorDigest,
	} {
		writeManifestString(digest, value)
	}
	writeManifestInteger(digest, int64(manifest.Dimensions))
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func validateGenerationManifest(manifest GenerationManifest) (string, error) {
	if strings.TrimSpace(manifest.RepositoryID) == "" ||
		strings.TrimSpace(manifest.GenerationID) == "" ||
		strings.TrimSpace(manifest.ScanPolicyVersion) == "" ||
		!validManifestSHA256(manifest.CorpusRevision) ||
		!validManifestSHA256(manifest.ContentDigest) ||
		!validManifestSHA256(manifest.VectorDigest) ||
		manifest.Metric != VectorMetricCosine {
		return "", ErrInvalidGeneration
	}
	if manifest.ProfileFingerprint == "" && manifest.ProfileModel == "" && manifest.Dimensions == 0 {
		return "lexical-only", nil
	}
	if strings.TrimSpace(manifest.ProfileFingerprint) == "" ||
		strings.TrimSpace(manifest.ProfileModel) == "" || manifest.Dimensions <= 0 {
		return "", ErrInvalidGeneration
	}
	return "semantic", nil
}

func validManifestSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func writeManifestString(writer hash.Hash, value string) {
	writeManifestInteger(writer, int64(len(value)))
	_, _ = writer.Write([]byte(value))
}

func writeManifestInteger(writer hash.Hash, value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = writer.Write(encoded[:])
}
