package board_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

const crashReplay = "REB_TEST_CRASH_REPLAY"

func TestAC01101CommittedOrPreviewOnlyAfterProcessExit(t *testing.T) {
	for _, scenario := range []struct {
		name string
		want string
	}{
		{"committed", board.Sufficient},
		{"preview_only", board.Blocked},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "crash-check.db")
			ctx, cancel := crashChildContext(t)
			defer cancel()
			output, err := runCrashTestChild(ctx, "TestCrashFixture", crashReplay+"="+scenario.name, "REB_TEST_CRASH_FILE="+path)
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 73 {
				t.Fatalf("fixture did not reach abrupt exit: %v %s", err, output)
			}
			app, err := board.Open(path, board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return fixtureTime }})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := app.Close(); err != nil {
					t.Error(err)
				}
			}()
			candidates, err := app.Candidates(supervisor, project)
			if err != nil || len(candidates) != 1 {
				t.Fatalf("candidate state after process exit: %#v %v", candidates, err)
			}
			actual, err := app.Assess(supervisor, project, candidates[0].ID)
			if err != nil || actual.Status != scenario.want || len(actual.Behaviors) != 1 {
				t.Fatalf("assessment after process exit = %#v %v", actual, err)
			}
			switch scenario.name {
			case "committed":
				if len(actual.Behaviors[0].Runs) != 1 || actual.LedgerHighWater != 1 {
					t.Fatalf("run or ledger missing after committed process exit: %#v", actual)
				}
				run := actual.Behaviors[0].Runs[0]
				stamp := board.FormatUTC(fixtureTime)
				expected := resultRow(candidates[0], "T1", "1", "RUN1", "RES1", stamp, stamp, "PASS")
				raw := csvBytes(t, expected)
				digest := sha256.Sum256(raw)
				if run.ID != "RUN1" || run.Sequence != 1 || run.CandidateID != candidates[0].ID ||
					len(run.Rows) != 1 || !slices.Equal(run.Rows[0].Fields[:], expected) {
					t.Fatal("restart changed the committed run, row or original ledger sequence")
				}
				receipt, err := app.Receipt(supervisor, project, run.ReceiptID)
				if err != nil || receipt.ID != run.ReceiptID || receipt.Hash != hex.EncodeToString(digest[:]) ||
					receipt.Rows != 1 || receipt.LedgerHighWater != 1 || receipt.OpenCandidateID != candidates[0].ID ||
					!slices.Equal(receipt.AddedRuns, []string{"RUN1"}) || !slices.Equal(receipt.CandidateIDs, []string{candidates[0].ID}) {
					t.Fatalf("restart did not preserve the receipt bound to the committed source and ledger: %v", err)
				}
				original, err := app.SourceCSV(supervisor, project, receipt.ID)
				if err != nil || !bytes.Equal(original, raw) {
					t.Fatalf("restart changed the exact original bytes: %v", err)
				}
				recordApprovalInstance(t, "AC-011-01.P001", map[string]any{}, "Same rows, receipt and ledger sequence restored")
			default:
				if len(actual.Behaviors[0].Runs) != 0 || actual.LedgerHighWater != 0 {
					t.Fatalf("preview-only process exit created evidence: %#v", actual)
				}
			}
		})
	}
}

func TestCrashFixture(t *testing.T) {
	kind := os.Getenv(crashReplay)
	if kind == "" {
		return
	}
	app, err := board.Open(os.Getenv("REB_TEST_CRASH_FILE"), board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return fixtureTime }})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Bootstrap(supervisor, testPassword); err != nil {
		t.Fatal(err)
	}
	candidate := fixtureCandidate(t, app, vectorBaseline())
	stamp := board.FormatUTC(fixtureTime)
	raw := csvBytes(t, resultRow(candidate, "T1", "1", "RUN1", "RES1", stamp, stamp, "PASS"))
	switch kind {
	case "committed":
		importBytes(t, app, candidate.ID, raw)
	case "preview_only":
		preview, err := app.PreviewImport(supervisor, project, candidate.ID, raw)
		if err != nil || preview.Token == "" || len(preview.Issues) != 0 {
			t.Fatalf("staged preview = %#v %v", preview, err)
		}
	default:
		t.Fatalf("unknown fixture action %q", kind)
	}
	// Terminate immediately with no deferred Close to exercise OS recovery.
	os.Exit(73)
}
