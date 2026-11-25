//go:build windows

package board_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
	"release-evidence-board/internal/board"
)

func TestTicket85HealthDetectsNativeWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "write-health.db")
	app, err := board.Open(path, board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if app != nil {
			if err := app.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	if err := app.Bootstrap("reviewer", "synthetic-write-health-password"); err != nil {
		t.Fatal(err)
	}
	prior, err := app.CreateDraft("reviewer", "pilot", board.Baseline{ID: "Prior", ProductID: "DEMO"}, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if check := app.Check(); !check.DatabaseOK {
		t.Fatalf("healthy database failed initial check: %+v", check)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	// Native shared byte locks deny writes but allow reads, including mapped
	// reads. This range does not overlap bbolt's lock at offset -1.
	offset := windows.Overlapped{}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1<<30, 0, &offset); err != nil {
		t.Fatal(err)
	}
	locked := true
	t.Cleanup(func() {
		if locked {
			if err := windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1<<30, 0, &offset); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := app.Draft("reviewer", "pilot", prior.ID); err != nil {
		t.Fatalf("native write fault unexpectedly prevented retained reads: %v", err)
	}
	_, writeError := app.CreateDraft("reviewer", "pilot", board.Baseline{ID: "DuringFault", ProductID: "DEMO"}, board.DraftOptions{})
	if writeError == nil {
		t.Fatal("native file lock did not cause an actual write failure")
	}
	if !errors.Is(writeError, windows.ERROR_LOCK_VIOLATION) {
		t.Fatalf("fault did not reach the expected native write refusal: %v", writeError)
	}
	t.Logf("native write failure observed: %v", writeError)
	for failures := 1; failures <= 3; failures++ {
		check := app.Check()
		if check.DatabaseOK || check.ConsecutiveFailures != failures {
			t.Fatalf("readable but unwritable database was reported healthy: %+v, want %d failures", check, failures)
		}
	}
	if err := windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1<<30, 0, &offset); err != nil {
		t.Fatal(err)
	}
	locked = false
	if check := app.Check(); !check.DatabaseOK || check.ConsecutiveFailures != 0 {
		t.Fatalf("writable database did not recover: %+v", check)
	}
	_, drafts, err := app.Baselines("reviewer", "pilot")
	if err != nil || len(drafts) != 1 || drafts[0].ID != prior.ID {
		t.Fatalf("failed write changed retained drafts: %v %+v", err, drafts)
	}
	if _, err := app.CreateDraft("reviewer", "pilot", board.Baseline{ID: "AfterFault", ProductID: "DEMO"}, board.DraftOptions{}); err != nil {
		t.Fatalf("draft creation after fault removal failed: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	app = nil
	app, err = board.Open(path, board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	_, drafts, err = app.Baselines("reviewer", "pilot")
	if err != nil || len(drafts) != 2 {
		t.Fatalf("restart did not preserve exactly the two successful drafts: %v %+v", err, drafts)
	}
}
