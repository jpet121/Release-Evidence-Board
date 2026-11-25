package board_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

const interruptionDatabase = "REB_FOLLOWUP_INTERRUPTION_DATABASE"
const interruptionMode = "REB_FOLLOWUP_INTERRUPTION_MODE"

func TestFollowupIntakeInterruptionRollsBackOnlyUncommittedImport(t *testing.T) {
	for _, trial := range []struct {
		name   string
		marker string
		exit   int
		status string
		ledger uint64
	}{
		{"interrupted", "new import reached its transactional audit clock", 74, "SUFFICIENT", 1},
		{"committed", "new import and its audit returned successfully", 75, "BLOCKED", 2},
	} {
		t.Run(trial.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "interrupt.db")
			ctx, cancel := crashChildContext(t)
			defer cancel()
			output, err := runCrashTestChild(ctx, "TestFollowupIntakeChildProcess",
				interruptionDatabase+"="+path, interruptionMode+"="+trial.name)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != trial.exit || !bytes.Contains(output, []byte(trial.marker)) {
				t.Fatalf("child did not reach exact interruption stage: %v, %s", err, output)
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
				t.Fatalf("restart lost the fixture candidate: %v", err)
			}
			candidate := candidates[0]
			state := statusAssessment(t, app, candidate.ID, trial.status, trial.ledger)
			controlRaw, proposedRaw := interruptionRows(t, candidate)
			assertDurableImport(t, app, controlRaw, 1, "PRESERVED")
			if trial.ledger == 2 {
				assertDurableImport(t, app, proposedRaw, 2, "BEFORE-EXIT")
				if state.Behaviors[0].CurrentRunID != "BEFORE-EXIT" {
					t.Fatal("committed failure run was not current after restart")
				}
			} else {
				id := fmt.Sprintf("I-%x", sha256.Sum256(proposedRaw))
				if _, err := app.Receipt(supervisor, project, id); !errors.Is(err, board.ErrNotFound) {
					t.Fatalf("interrupted transaction exposed a receipt: %v", err)
				}
				if _, err := app.SourceCSV(supervisor, project, id); !errors.Is(err, board.ErrNotFound) {
					t.Fatalf("interrupted transaction exposed an original: %v", err)
				}
				if state.Behaviors[0].CurrentRunID != "PRESERVED" || len(state.Behaviors[0].Runs) != 1 {
					t.Fatal("interrupted failure run leaked into decision history")
				}
			}
			events := intakeAudit(t, app)
			success := 0
			for _, event := range events {
				if event.Action == "import.commit" && event.Outcome == "success" {
					success++
				}
			}
			if success != int(trial.ledger) {
				t.Fatalf("restart has %d successful import events at ledger %d", success, trial.ledger)
			}
			if trial.name == "committed" {
				recordApprovalInstance(t, "AC-005-01.P001", map[string]any{}, "Rows, ledger sequences, receipt and audit commit together")
				recordApprovalInstance(t, "AC-012-01.P002", map[string]any{"authorizedAction": "import"}, "Actor, UTC time, action, references and outcome recorded")
			} else {
				recordApprovalInstance(t, "AC-005-02.P001", map[string]any{}, "No committed partial import or success event")
			}
			t.Logf("child_exit=%d state=%s ledger=%d material_success_events=%d", trial.exit, state.Status, state.LedgerHighWater, success)
		})
	}
}

func TestFollowupIntakeChildProcess(t *testing.T) {
	path := os.Getenv(interruptionDatabase)
	if path == "" {
		return
	}
	mode := os.Getenv(interruptionMode)
	armed, calls := false, 0
	clock := func() time.Time {
		if armed {
			calls++
			if calls == 2 {
				fmt.Println("new import reached its transactional audit clock")
				os.Exit(74)
			}
		}
		return fixtureTime
	}
	app, err := board.Open(path, board.Settings{ProjectID: project, ProductID: "DEMO", Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Bootstrap(supervisor, testPassword); err != nil {
		t.Fatal(err)
	}
	candidate := fixtureCandidate(t, app, vectorBaseline())
	controlRaw, proposedRaw := interruptionRows(t, candidate)
	importBytes(t, app, candidate.ID, controlRaw)
	preview := previewFollowupFile(t, app, candidate.ID, proposedRaw)
	switch mode {
	case "interrupted":
		armed = true
	case "committed":
	default:
		t.Fatalf("unsupported child mode %q", mode)
	}
	receipt, err := app.CommitImport(supervisor, project, preview.Token, proposedRaw)
	if err != nil || receipt.LedgerHighWater != 2 {
		t.Fatalf("child did not commit its valid fixture: %v", err)
	}
	if armed {
		t.Fatal("import returned before the in-transaction interruption")
	}
	fmt.Println("new import and its audit returned successfully")
	os.Exit(75)
}

func interruptionRows(t *testing.T, candidate board.Candidate) ([]byte, []byte) {
	t.Helper()
	return csvBytes(t, resultRow(candidate, "T1", "1", "PRESERVED", "GOOD-RESULT", intakeEarlier, intakeEarlier, "PASS")),
		csvBytes(t, resultRow(candidate, "T1", "1", "BEFORE-EXIT", "FAIL-RESULT", intakeCurrent, intakeCurrent, "FAIL"))
}

func assertDurableImport(t *testing.T, app *board.Board, raw []byte, sequence uint64, run string) {
	t.Helper()
	id := fmt.Sprintf("I-%x", sha256.Sum256(raw))
	receipt, err := app.Receipt(supervisor, project, id)
	if err != nil || receipt.LedgerHighWater != sequence || !slices.Equal(receipt.AddedRuns, []string{run}) || receipt.Uploader != supervisor {
		t.Fatalf("restart lost a committed receipt, run identity or ledger sequence: %v", err)
	}
	retained, err := app.SourceCSV(supervisor, project, id)
	if err != nil || !bytes.Equal(retained, raw) {
		t.Fatalf("restart lost a committed producer original: %v", err)
	}
	if !slices.ContainsFunc(intakeAudit(t, app), func(event board.AuditEvent) bool {
		return event.Action == "import.commit" && event.Outcome == "success" && event.Actor == supervisor &&
			event.Time.Equal(fixtureTime) && event.Time.Location() == time.UTC && slices.Contains(event.References, id)
	}) {
		t.Fatal("restart lost the attributed audit for a committed receipt")
	}
}
