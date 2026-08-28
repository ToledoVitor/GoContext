//go:build !windows

package sqlite

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishStoreIdentitySidecarIsExclusive(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, storeIdentitySidecar)
	firstTemporary := filepath.Join(directory, "first.tmp")
	firstPayload := []byte("first identity")
	if err := os.WriteFile(firstTemporary, firstPayload, 0o600); err != nil {
		t.Fatalf("WriteFile(first temporary) error = %v", err)
	}
	if err := publishStoreIdentitySidecar(firstTemporary, target, directory); err != nil {
		t.Fatalf("publishStoreIdentitySidecar(first) error = %v", err)
	}
	if _, err := os.Lstat(firstTemporary); !os.IsNotExist(err) {
		t.Fatalf("Lstat(first temporary) error = %v, want removed", err)
	}

	secondTemporary := filepath.Join(directory, "second.tmp")
	if err := os.WriteFile(secondTemporary, []byte("second identity"), 0o600); err != nil {
		t.Fatalf("WriteFile(second temporary) error = %v", err)
	}
	if err := publishStoreIdentitySidecar(secondTemporary, target, directory); err == nil {
		t.Fatal("publishStoreIdentitySidecar(second) error = nil, want exclusive collision")
	}
	payload, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target) error = %v", err)
	}
	if !bytes.Equal(payload, firstPayload) {
		t.Fatalf("target payload = %q, want original %q", payload, firstPayload)
	}
}
