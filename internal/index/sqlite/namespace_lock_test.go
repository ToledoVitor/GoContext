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
	creatorEntered := make(chan struct{})
	releaseCreator := make(chan struct{})
	creatorResult := make(chan error, 1)
	go func() {
		store, err := newStore(directory, storeOpenHooks{
			beforeFreshIdentityInsert: func(string) error {
				close(creatorEntered)
				<-releaseCreator
				return nil
			},
		})
		if store != nil {
			err = errors.Join(err, store.Close())
		}
		creatorResult <- err
	}()
	<-creatorEntered

	var command *exec.Cmd
	commandFinished := false
	t.Cleanup(func() {
		select {
		case <-releaseCreator:
		default:
			close(releaseCreator)
		}
		if !commandFinished && command != nil && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	command = exec.Command(os.Args[0], "-test.run=^TestStoreNamespaceLockHelperProcess$")
	command.Env = append(
		os.Environ(),
		namespaceLockHelperEnvironment+"=open-existing",
		namespaceLockDirectoryEnv+"="+directory,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() error = %v", err)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("Start(subprocess opener) error = %v", err)
	}
	output := bufio.NewReader(stdout)
	type outputResult struct {
		line string
		err  error
	}
	waitingResult := make(chan outputResult, 1)
	go func() {
		line, err := output.ReadString('\n')
		waitingResult <- outputResult{line: line, err: err}
	}()
	select {
	case result := <-waitingResult:
		if result.err != nil || strings.TrimSpace(result.line) != "waiting" {
			t.Fatalf("subprocess opener boundary = (%q, %v), want waiting", result.line, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subprocess opener did not reach the live OS lock wait")
	}
	openedResult := make(chan outputResult, 1)
	go func() {
		line, err := output.ReadString('\n')
		openedResult <- outputResult{line: line, err: err}
	}()
	select {
	case result := <-openedResult:
		t.Fatalf("subprocess OpenExisting returned before first creator published: (%q, %v)", result.line, result.err)
	default:
	}
	close(releaseCreator)
	if err := <-creatorResult; err != nil {
		t.Fatalf("newStore(first creator) error = %v", err)
	}
	select {
	case result := <-openedResult:
		if result.err != nil || strings.TrimSpace(result.line) != "opened" {
			t.Fatalf("subprocess opener completion = (%q, %v), want opened", result.line, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subprocess OpenExisting remained blocked after first creator published")
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
	if mode == "open-existing" {
		store, err := openExisting(directory, openExistingHooks{
			namespaceLock: storeNamespaceLockHooks{
				beforeFileLock: func(string) error {
					_, err := fmt.Fprintln(os.Stdout, "waiting")
					return err
				},
			},
		})
		if err != nil {
			t.Fatalf("openExisting(subprocess opener) error = %v", err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("Close(subprocess opener) error = %v", err)
		}
		if _, err := fmt.Fprintln(os.Stdout, "opened"); err != nil {
			t.Fatalf("announce subprocess opener completion error = %v", err)
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
