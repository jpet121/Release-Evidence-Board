package board_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

const (
	crashChildTimeout   = 15 * time.Second
	crashChildWaitDelay = time.Second
	stalledChildDB      = "REB_TEST_STALLED_CRASH_CHILD_DB"
	stalledReady        = "board crash child acquired its database"
)

func crashChildContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	deadline := time.Now().Add(crashChildTimeout)
	if parentDeadline, ok := t.Deadline(); ok {
		// Leave time to reap the child and drain its output before the
		// parent Go test watchdog can terminate this test process.
		cleanupDeadline := parentDeadline.Add(-2 * crashChildWaitDelay)
		if cleanupDeadline.Before(deadline) {
			deadline = cleanupDeadline
		}
	}
	return context.WithDeadline(t.Context(), deadline)
}

func runCrashTestChild(ctx context.Context, name string, environment ...string) ([]byte, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	child := exec.CommandContext(ctx, executable, "-test.run=^"+name+"$", "-test.timeout="+crashChildTimeout.String())
	child.Env = append(child.Environ(), environment...)
	child.WaitDelay = crashChildWaitDelay
	output, err := child.CombinedOutput()
	if err != nil && ctx.Err() != nil {
		return output, errors.Join(ctx.Err(), err)
	}
	return output, err
}

func TestCrashChildSupervisorCancelsAndReapsOwnedProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stalled.db")
	ctx, cancel := crashChildContext(t)
	type outcome struct {
		output []byte
		err    error
	}
	result := make(chan outcome, 1)
	finished := false
	defer func() {
		cancel()
		if finished {
			return
		}
		// This escape joins the negative control even with the original
		// unbounded launch. The child's own watchdog backs it up.
		if err := os.WriteFile(path+".release", nil, 0600); err != nil {
			t.Errorf("release the stalled control: %v", err)
		}
		select {
		case <-result:
			t.Log("fallback release joined the owned control child")
		case <-time.After(12 * time.Second):
			t.Error("stalled control did not join after its fallback release")
		}
	}()
	go func() {
		output, err := runCrashTestChild(ctx, "TestStalledCrashChildFixture", stalledChildDB+"="+path)
		result <- outcome{output, err}
	}()

	readyTimeout := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path + ".ready"); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case exited := <-result:
			finished = true
			t.Fatalf("control child exited before publishing readiness: %v %s", exited.err, exited.output)
		case <-readyTimeout:
			t.Fatal("stalled control never acquired the temporary database")
		case <-tick.C:
		}
	}

	started := time.Now()
	cancel()
	select {
	case exited := <-result:
		finished = true
		var childExit *exec.ExitError
		if !errors.Is(exited.err, context.Canceled) || !errors.As(exited.err, &childExit) ||
			childExit.Success() || !bytes.Contains(exited.output, []byte(stalledReady)) {
			t.Fatalf("cancellation did not terminate and reap the owned child with its readiness output: %v %s", exited.err, exited.output)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceling the test-owned context left the direct child running")
	}
	// A new Board acquires the same exclusive database lock after the
	// child is waited. The normal path never issues the fallback release.
	if _, err := os.Stat(path + ".release"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned child required the fallback release: %v", err)
	}
	reopened, err := board.Open(path, board.Settings{ProjectID: project, ProductID: "DEMO"})
	if err != nil {
		t.Fatalf("canceled child retained the database lock: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("owned direct child canceled, exited, waited and database reopened in %s", time.Since(started))
}

func TestStalledCrashChildFixture(t *testing.T) {
	path := os.Getenv(stalledChildDB)
	if path == "" {
		return
	}
	// The negative control needs a finite exit even when the launcher
	// ignores cancellation and the fallback release cannot be written.
	watchdog := time.AfterFunc(10*time.Second, func() { os.Exit(76) })
	defer watchdog.Stop()
	app, err := board.Open(path, board.Settings{ProjectID: project, ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	}()
	fmt.Println(stalledReady)
	if err := os.WriteFile(path+".ready", nil, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(path + ".release"); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
