//go:build aix || android || darwin || dragonfly || freebsd || hurd || illumos || ios || linux || netbsd || openbsd || solaris || zos

package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestScannerDoesNotBlockWhenFileIsSwappedToFIFO(t *testing.T) {
	root := t.TempDir()
	writeInternalFile(t, root, "allowed.py", "print('safe')\n")
	fifoPath := filepath.Join(root, "allowed.py")
	swapped := false
	scanner := &Scanner{
		openPath: func(directory repositoryHandle, name string, wantDirectory bool) (repositoryHandle, error) {
			if !wantDirectory && name == "allowed.py" && !swapped {
				swapped = true
				if err := os.Remove(fifoPath); err != nil {
					return nil, err
				}
				if err := unix.Mkfifo(fifoPath, 0o600); err != nil {
					return nil, err
				}
			}
			return openNoFollow(directory, name, wantDirectory)
		},
	}

	done := make(chan error, 1)
	go func() {
		_, err := scanner.Scan(context.Background(), root)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "file-changed") {
			t.Fatalf("Scan() error = %v, want fail-closed file-changed category", err)
		}
	case <-time.After(2 * time.Second):
		writer, _ := os.OpenFile(fifoPath, os.O_WRONLY|unix.O_NONBLOCK, 0)
		if writer != nil {
			_ = writer.Close()
		}
		t.Fatal("Scan() blocked opening FIFO swapped in for a regular file")
	}
}
