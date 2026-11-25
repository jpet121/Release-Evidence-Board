//go:build linux && reb_storage_fault

package board_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"release-evidence-board/internal/board"
)

func TestTicket85HealthDetectsNativeWriteFailure(t *testing.T) {
	if os.Getenv("REB_TICKET85_FSIZE_CHILD") != "1" {
		// Limit only a disposable test child. No other test, server, or host
		// process inherits the fault's process-wide file-size limit.
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child := exec.CommandContext(t.Context(), exe, "-test.run=^TestTicket85HealthDetectsNativeWriteFailure$", "-test.v")
		child.Env = append(os.Environ(), "REB_TICKET85_FSIZE_CHILD=1")
		output, err := child.CombinedOutput()
		t.Log(string(output))
		if err != nil {
			t.Fatalf("native write-limit child failed: %v", err)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "write-health.db")
	settings := board.Settings{ProjectID: "pilot", ProductID: "DEMO"}
	app, err := board.Open(path, settings)
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
		t.Fatalf("healthy initial check: %+v", check)
	}
	var previous syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
		t.Fatal(err)
	}
	limited := previous
	limited.Cur = 0
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited); err != nil {
		t.Fatal(err)
	}
	active := true
	t.Cleanup(func() {
		if active {
			if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := app.Draft("reviewer", "pilot", prior.ID); err != nil {
		t.Fatalf("retained read under write fault: %v", err)
	}
	_, writeError := app.CreateDraft("reviewer", "pilot", board.Baseline{ID: "DuringFault", ProductID: "DEMO"}, board.DraftOptions{})
	if !errors.Is(writeError, syscall.EFBIG) {
		t.Fatalf("expected native EFBIG write refusal: %v", writeError)
	}
	t.Logf("native write failure observed: %v", writeError)
	for failures := 1; failures <= 3; failures++ {
		if check := app.Check(); check.DatabaseOK || check.ConsecutiveFailures != failures {
			t.Fatalf("readable but unwritable database health: %+v, want %d failures", check, failures)
		}
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
		t.Fatal(err)
	}
	active = false
	if check := app.Check(); !check.DatabaseOK || check.ConsecutiveFailures != 0 {
		t.Fatalf("writable storage recovery: %+v", check)
	}
	_, drafts, err := app.Baselines("reviewer", "pilot")
	if err != nil || len(drafts) != 1 || drafts[0].ID != prior.ID {
		t.Fatalf("failed write changed drafts: %v %+v", err, drafts)
	}
	if _, err := app.CreateDraft("reviewer", "pilot", board.Baseline{ID: "AfterFault", ProductID: "DEMO"}, board.DraftOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	app = nil
	app, err = board.Open(path, settings)
	if err != nil {
		t.Fatal(err)
	}
	_, drafts, err = app.Baselines("reviewer", "pilot")
	if err != nil || len(drafts) != 2 {
		t.Fatalf("restart lost successful writes: %v %+v", err, drafts)
	}
}
