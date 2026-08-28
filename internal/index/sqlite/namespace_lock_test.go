package sqlite

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	namespaceLockHelperEnvironment = "GOCONTEXT_SQLITE_LOCK_HELPER"
	namespaceLockDirectoryEnv      = "GOCONTEXT_SQLITE_LOCK_DIRECTORY"
)

func TestStoreNamespaceLockSerializesAcrossProcesses(t *testing.T) {
	directory := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestStoreNamespaceLockHelperProcess$")
	command.Env = append(
		os.Environ(),
		namespaceLockHelperEnvironment+"=1",
		namespaceLockDirectoryEnv+"="+directory,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe() error = %v", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() error = %v", err)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("Start(lock helper) error = %v", err)
	}
	commandFinished := false
	t.Cleanup(func() {
		_ = stdin.Close()
		if !commandFinished && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})

	ready := make(chan error, 1)
	go func() {
		line, readErr := bufio.NewReader(stdout).ReadString('\n')
		if readErr == nil && strings.TrimSpace(line) != "locked" {
			readErr = fmt.Errorf("lock helper output = %q", line)
		}
		ready <- readErr
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("lock helper readiness error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lock helper did not acquire namespace lock")
	}

	acquired := make(chan *storeNamespaceLock, 1)
	acquireErr := make(chan error, 1)
	go func() {
		lock, err := acquireStoreNamespaceLock(directory, false)
		if err != nil {
			acquireErr <- err
			return
		}
		acquired <- lock
	}()
	select {
	case lock := <-acquired:
		_ = lock.release()
		t.Fatal("second process acquired namespace lock before helper released it")
	case err := <-acquireErr:
		t.Fatalf("second process lock acquisition error = %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := stdin.Write([]byte("release\n")); err != nil {
		t.Fatalf("release lock helper error = %v", err)
	}
	select {
	case lock := <-acquired:
		if err := lock.release(); err != nil {
			t.Fatalf("release contender namespace lock error = %v", err)
		}
	case err := <-acquireErr:
		t.Fatalf("contender namespace lock acquisition error = %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("contender remained blocked after helper released namespace lock")
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("Wait(lock helper) error = %v", err)
	}
	commandFinished = true
}

func TestStoreNamespaceLockHelperProcess(t *testing.T) {
	if os.Getenv(namespaceLockHelperEnvironment) != "1" {
		return
	}
	directory := os.Getenv(namespaceLockDirectoryEnv)
	lock, err := acquireStoreNamespaceLock(directory, true)
	if err != nil {
		t.Fatalf("acquireStoreNamespaceLock(helper) error = %v", err)
	}
	if _, err := fmt.Fprintln(os.Stdout, "locked"); err != nil {
		_ = lock.release()
		t.Fatalf("announce helper lock error = %v", err)
	}
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		_ = lock.release()
		t.Fatalf("wait for helper release error = %v", err)
	}
	if err := lock.release(); err != nil {
		t.Fatalf("release helper namespace lock error = %v", err)
	}
}
