package sqlite

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ToledoVitor/GoContext/internal/index"
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

func TestOpenExistingSerializesWithFirstCreatorAcrossProcesses(t *testing.T) {
	directory := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestStoreNamespaceLockHelperProcess$")
	command.Env = append(
		os.Environ(),
		namespaceLockHelperEnvironment+"=first-creator",
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
		t.Fatalf("Start(first creator helper) error = %v", err)
	}
	commandFinished := false
	t.Cleanup(func() {
		_ = stdin.Close()
		if !commandFinished && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	output := bufio.NewReader(stdout)
	line, err := output.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "creating" {
		t.Fatalf("first creator readiness = (%q, %v), want creating", line, err)
	}

	type openResult struct {
		store *Store
		err   error
	}
	openerResult := make(chan openResult, 1)
	go func() {
		store, err := OpenExisting(directory)
		openerResult <- openResult{store: store, err: err}
	}()
	select {
	case result := <-openerResult:
		if result.store != nil {
			_ = result.store.Close()
		}
		t.Fatalf("OpenExisting() returned before subprocess creator published: %v", result.err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := stdin.Write([]byte("publish\n")); err != nil {
		t.Fatalf("release first creator helper error = %v", err)
	}
	select {
	case result := <-openerResult:
		if result.err != nil || result.store == nil {
			t.Fatalf("OpenExisting(after subprocess publication) = (%v, %v), want store", result.store, result.err)
		}
		if err := result.store.Close(); err != nil {
			t.Fatalf("Close(opened store) error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OpenExisting() remained blocked after subprocess creator published")
	}
	line, err = output.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "created" {
		t.Fatalf("first creator completion = (%q, %v), want created", line, err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("Wait(first creator helper) error = %v", err)
	}
	commandFinished = true
}

func TestOpenExistingContextCancellationInterruptsCrossProcessLockWait(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod(directory) error = %v", err)
	}
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
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("lock helper readiness = (%q, %v), want locked", line, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	openerWaiting := make(chan struct{})
	openerResult := make(chan error, 1)
	go func() {
		store, err := openExistingContext(ctx, directory, openExistingHooks{
			namespaceLock: storeNamespaceLockHooks{
				beforeFileLock: func(string) error {
					close(openerWaiting)
					return nil
				},
			},
		})
		if store != nil {
			err = errors.Join(err, store.Close())
		}
		openerResult <- err
	}()
	<-openerWaiting
	cancel()
	select {
	case err := <-openerResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("OpenExistingContext(cross-process wait) error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OpenExistingContext() did not return after cross-process wait cancellation")
	}
	if _, err := stdin.Write([]byte("release\n")); err != nil {
		t.Fatalf("release lock helper error = %v", err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("Wait(lock helper) error = %v", err)
	}
	commandFinished = true
	if _, err := OpenExisting(directory); !errors.Is(err, index.ErrNotFound) {
		t.Fatalf("OpenExisting(after canceled cross-process wait) error = %v, want ErrNotFound", err)
	}
}

func TestStoreNamespaceLockHelperProcess(t *testing.T) {
	mode := os.Getenv(namespaceLockHelperEnvironment)
	if mode == "" {
		return
	}
	directory := os.Getenv(namespaceLockDirectoryEnv)
	if mode == "first-creator" {
		store, err := newStore(directory, storeOpenHooks{
			beforeFreshIdentityInsert: func(string) error {
				if _, err := fmt.Fprintln(os.Stdout, "creating"); err != nil {
					return err
				}
				_, err := bufio.NewReader(os.Stdin).ReadString('\n')
				return err
			},
		})
		if err != nil {
			t.Fatalf("newStore(first creator helper) error = %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("Close(first creator helper) error = %v", err)
		}
		if _, err := fmt.Fprintln(os.Stdout, "created"); err != nil {
			t.Fatalf("announce first creator completion error = %v", err)
		}
		return
	}
	if mode != "1" {
		return
	}
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
