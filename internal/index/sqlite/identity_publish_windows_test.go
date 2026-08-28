//go:build windows

package sqlite

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestIdentitySidecarMoveIsExclusiveAndWriteThrough(t *testing.T) {
	if identitySidecarMoveFlags&windows.MOVEFILE_WRITE_THROUGH == 0 {
		t.Fatal("identity sidecar publication is not write-through")
	}
	if identitySidecarMoveFlags&windows.MOVEFILE_REPLACE_EXISTING != 0 {
		t.Fatal("identity sidecar publication can replace an existing identity")
	}
}
