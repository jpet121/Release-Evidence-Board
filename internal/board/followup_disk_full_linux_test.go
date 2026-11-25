//go:build linux && reb_storage_fault

package board_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"release-evidence-board/internal/board"
)

func TestFollowupCommitFailsAtActualFullStorage(t *testing.T) {
	root := os.Getenv("REB_PRIVATE_ENOSPC_ROOT")
	if filepath.Clean(root) != root || filepath.Dir(root) != "/tmp" || !strings.HasPrefix(filepath.Base(root), "reb-enospc.") {
		t.Fatal("this opt-in test needs the runner's fresh private tmpfs")
	}
	var mounted, parent unix.Stat_t
	if err := unix.Stat(root, &mounted); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(filepath.Dir(root), &parent); err != nil || mounted.Dev == parent.Dev {
		t.Fatal("fault directory is not a separate mounted device")
	}
	info := inspectFaultVolume(t, root)
	if info.Type != unix.TMPFS_MAGIC || info.Blocks*uint64(info.Bsize) != 16<<20 || info.Bavail != info.Blocks {
		t.Fatalf("expected a fresh dedicated 16 MiB tmpfs: %#v", info)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("private test mount is occupied: %v", err)
	}
	t.Logf("actual Linux tmpfs: %d total and %d available blocks at %d bytes", info.Blocks, info.Bavail, info.Bsize)

	file := filepath.Join(root, "evidence.db")
	now := fixtureTime
	settings := board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return now }}
	app, err := board.Open(file, settings)
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
	if err := app.Bootstrap(supervisor, testPassword); err != nil {
		t.Fatal(err)
	}
	prior := fixtureCandidate(t, app, vectorBaseline())
	stamp := board.FormatUTC(now)
	committed := csvBytes(t, resultRow(prior, "T1", "1", "KEPT", "KEPT-RESULT", stamp, stamp, "PASS"))
	receipt := importBytes(t, app, prior.ID, committed)
	frozen, err := app.Freeze(supervisor, project, prior.ID)
	if err != nil || frozen.Status != board.Sufficient || frozen.LedgerHighWater != 1 {
		t.Fatalf("intact historical fixture did not produce its genuine sufficient snapshot: %v", err)
	}
	_, priorHTML, err := app.ReportHTML(supervisor, project, frozen.ID)
	if err != nil {
		t.Fatal(err)
	}
	next := newCustodySuccessor(t, app, prior, "a")
	rows := make([][]string, 0, 768)
	for index := range 768 {
		runID := fmt.Sprintf("NO-SPACE-RUN-%04d", index)
		resultID := fmt.Sprintf("NO-SPACE-RESULT-%04d", index)
		row := resultRow(next, "T1", "1", runID, resultID, stamp, stamp, "PASS")
		row[17] = "https://example.invalid/retained/" + strings.Repeat("x", 160)
		rows = append(rows, row)
	}
	passing := csvBytes(t, rows...)
	staged, err := app.PreviewImport(supervisor, project, next.ID, passing)
	if err != nil || staged.Token == "" || len(staged.Issues) != 0 ||
		len(staged.Projected) != 1 || staged.Projected[0].Status != board.Sufficient {
		t.Fatalf("valid staging needed to demonstrate possible false sufficiency: token=%t issues=%d error=%v", staged.Token != "", len(staged.Issues), err)
	}
	pressure := fillFaultVolume(t, root)
	_, failed := app.CommitImport(supervisor, project, staged.Token, passing)
	if !errors.Is(failed, syscall.ENOSPC) {
		t.Fatalf("real full-volume evidence commit returned %v, want native ENOSPC", failed)
	}
	t.Logf("observed real commit ENOSPC while private volume was full: %v", failed)
	if err := os.Remove(pressure); err != nil {
		t.Fatal(err)
	}
	if after := inspectFaultVolume(t, root); after.Bavail == 0 {
		t.Fatal("failed to release the private filler")
	}

	checkPreserved := func() {
		t.Helper()
		missingReceipt := "I-" + custodyDigest(passing)
		if _, err := app.Receipt(supervisor, project, missingReceipt); !errors.Is(err, board.ErrNotFound) {
			t.Fatalf("failed commit exposed a new receipt: %v", err)
		}
		if _, err := app.SourceCSV(supervisor, project, missingReceipt); !errors.Is(err, board.ErrNotFound) {
			t.Fatalf("failed commit exposed a committed original: %v", err)
		}
		status, err := app.Assess(supervisor, project, next.ID)
		if err != nil || status.Status != board.Blocked || status.LedgerHighWater != receipt.LedgerHighWater ||
			len(status.Behaviors) != 1 || len(status.Behaviors[0].Runs) != 0 || status.Behaviors[0].CurrentRunID != "" {
			t.Fatalf("failed commit created ledger entries or a sufficient decision: %v", err)
		}
		kept, err := app.Receipt(supervisor, project, receipt.ID)
		if err != nil || !reflect.DeepEqual(kept, receipt) {
			t.Fatalf("failed import changed the prior committed receipt: %v", err)
		}
		exact, err := app.SourceCSV(supervisor, project, receipt.ID)
		if err != nil || !bytes.Equal(exact, committed) {
			t.Fatalf("failed import changed prior original evidence: %v", err)
		}
		_, html, err := app.ReportHTML(supervisor, project, frozen.ID)
		if err != nil || !bytes.Equal(html, priorHTML) {
			t.Fatalf("failed import changed the original already frozen snapshot: %v", err)
		}
	}
	checkPreserved()
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	app = nil
	now = now.Add(31 * time.Minute)
	app, err = board.Open(file, settings)
	if err != nil {
		t.Fatal(err)
	}
	checkPreserved()
	if _, err := app.CommitImport(supervisor, project, staged.Token, passing); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("restart left expired failed staging usable: %v", err)
	}
	report, err := app.Freeze(supervisor, project, next.ID)
	if err != nil || report.Status != board.Blocked || report.LedgerHighWater != receipt.LedgerHighWater {
		t.Fatalf("recovered store emitted a false sufficient snapshot after ENOSPC: %v", err)
	}
	recordApprovalInstance(t, "AC-011-04.P001", map[string]any{}, "No PASS snapshot referring to absent evidence")
}

func inspectFaultVolume(t *testing.T, root string) unix.Statfs_t {
	t.Helper()
	var volume unix.Statfs_t
	if err := unix.Statfs(root, &volume); err != nil {
		t.Fatal(err)
	}
	return volume
}

func fillFaultVolume(t *testing.T, root string) string {
	t.Helper()
	filler, err := os.CreateTemp(root, "owned-pressure-")
	if err != nil {
		t.Fatal(err)
	}
	path := filler.Name()
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
		}
	})
	space := inspectFaultVolume(t, root)
	length := int64(space.Bavail * uint64(space.Bsize))
	if length <= 0 || length > 16<<20 {
		t.Fatalf("mounted fixture has unexpected free bytes %d", length)
	}
	if err := errors.Join(unix.Fallocate(int(filler.Fd()), 0, 0, length), filler.Close()); err != nil {
		t.Fatal(err)
	}
	if after := inspectFaultVolume(t, root); after.Bavail != 0 {
		t.Fatalf("private storage still has %d writable blocks", after.Bavail)
	}
	return path
}
