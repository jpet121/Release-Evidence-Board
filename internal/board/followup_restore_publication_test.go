package board_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
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

func TestFollowupRestorePublicationBlocksAssessment(t *testing.T) {
	for _, trialID := range []string{"restore-interrupted", "restore-committed"} {
		t.Run(trialID, func(t *testing.T) {
			dir := t.TempDir()
			fixture := createInterruptionFixture(t, dir)
			t.Cleanup(func() { assertRestoreSourceUnchanged(t, fixture.Backup) })
			encoded, err := json.Marshal(fixture)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "identity.json"), encoded, 0600); err != nil {
				t.Fatal(err)
			}
			trial := componentTrials[slices.IndexFunc(componentTrials, func(trial interruptionTrial) bool { return trial.ID == trialID })]
			ctx, cancel := crashChildContext(t)
			defer cancel()
			output, err := runCrashTestChild(ctx, "TestFollowupNativeComponentChild",
				componentInterruptionDir+"="+dir, componentInterruptionTrial+"="+trialID)
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != trial.Exit ||
				!bytes.Contains(output, []byte("REB_NATIVE_PHASE "+trial.Phase+"\n")) {
				t.Fatalf("expected owned restore interruption boundary %s and exit %d: %s; %v", trial.Phase, trial.Exit, output, err)
			}
			destination := filepath.Join(dir, "restored.db")
			_, statErr := os.Lstat(destination)
			if trialID == "restore-interrupted" && errors.Is(statErr, os.ErrNotExist) {
				t.Log("interruption left the requested destination absent; no store was opened at that path")
				return
			}
			if statErr != nil {
				t.Fatal(statErr)
			}
			app, err := board.Open(destination, componentClock(fixtureTime.Add(2*time.Hour)))
			if err != nil {
				t.Fatal(err)
			}
			defer componentClose(t, app)
			held, holdErr := app.AssessmentSuspended(supervisor, project)
			assessment, assessmentErr := app.Assess(supervisor, project, fixture.Kept)
			report, freezeErr := app.Freeze(supervisor, project, fixture.Kept)
			if holdErr != nil || !held || !errors.Is(assessmentErr, board.ErrIntegrity) || !errors.Is(freezeErr, board.ErrIntegrity) {
				t.Fatalf("before reviewer revalidation: held=%t hold_err=%v assessment=%q assess_err=%v new_report=%q report_status=%q freeze_err=%v",
					held, holdErr, assessment.Status, assessmentErr, report.ID, report.Status, freezeErr)
			}
			health, err := app.Health(supervisor, project)
			if err != nil || !health.Hold {
				t.Fatalf("the restored store did not expose the review hold in health: %t; %v", health.Hold, err)
			}
			assertComponentHTML(t, app, fixture.OldReport, fixture.OldHTML)
			assertComponentHTML(t, app, fixture.KeptReport, fixture.KeptHTML)
			if trialID == "restore-interrupted" {
				return
			}

			reviewer, err := app.Authenticate(supervisor, testPassword)
			if err != nil || !reviewer.CanReview() {
				t.Fatalf("restore reviewer could not reauthenticate: %v", err)
			}
			if err := app.Revalidate(reviewer.ID, project); err != nil {
				t.Fatal(err)
			}
			assessment, err = app.Assess(reviewer.ID, project, fixture.Kept)
			if err != nil || assessment.Status != board.Sufficient || assessment.LedgerHighWater != 2 {
				t.Fatalf("authenticated revalidation did not restore the intact assessment: %q ledger=%d; %v", assessment.Status, assessment.LedgerHighWater, err)
			}
			report, err = app.Freeze(reviewer.ID, project, fixture.Kept)
			if err != nil || report.Status != board.Sufficient || report.LedgerHighWater != 2 ||
				report.ID == "" || report.ID == fixture.KeptReport.ID || report.ID == fixture.OldReport.ID {
				t.Fatalf("revalidation did not allow a new snapshot: %q %q ledger=%d; %v", report.ID, report.Status, report.LedgerHighWater, err)
			}
			stored, html, err := app.ReportHTML(reviewer.ID, project, report.ID)
			if err != nil || stored.IntegrityWarning || fmt.Sprintf("%x", sha256.Sum256(html)) != report.SHA256 ||
				!bytes.Contains(html, []byte("INTERRUPT-KEPT-R")) {
				t.Fatalf("new snapshot could not reproduce the committed source evidence: %v", err)
			}
			assertComponentHTML(t, app, fixture.OldReport, fixture.OldHTML)
			assertComponentHTML(t, app, fixture.KeptReport, fixture.KeptHTML)
			assertComponentImport(t, app, fixture.OldCSV, 1, fixture.OldRun)
			assertComponentImport(t, app, fixture.KeptCSV, 2, fixture.KeptRun)
			t.Logf("restored assessment and freeze refused until authenticated reviewer revalidation; new report=%s ledger=%d and old bytes preserved", report.ID, report.LedgerHighWater)
		})
	}
}

func TestFollowupRestorePublicationPreservesConcurrentWinner(t *testing.T) {
	dir := t.TempDir()
	fixture := createInterruptionFixture(t, dir)
	t.Cleanup(func() { assertRestoreSourceUnchanged(t, fixture.Backup) })
	destination := filepath.Join(dir, "restored.db")
	at := fixtureTime.Add(2 * time.Hour)
	calls := 0
	var winner board.RestoreResult
	var winnerErr error
	inProgress := componentClock(at.Add(time.Hour))
	inProgress.Clock = func() time.Time {
		calls++
		if calls == 3 {
			// The first restore is inside its metadata transaction. Publish a
			// second public restore before letting the first continue.
			winner, winnerErr = board.RestoreBackup(fixture.Backup.Path, destination, componentClock(at))
		}
		return at.Add(time.Hour)
	}
	loser, loserErr := board.RestoreBackup(fixture.Backup.Path, destination, inProgress)
	if calls != 3 || winnerErr != nil || !winner.IntegrityOK || !winner.RestoredAt.Equal(at) ||
		!errors.Is(loserErr, os.ErrExist) || loser.IntegrityOK {
		t.Fatalf("first completing restore must own the destination: second timestamp=%v valid=%t err=%v; first valid=%t err=%v",
			winner.RestoredAt, winner.IntegrityOK, winnerErr, loser.IntegrityOK, loserErr)
	}
	app, err := board.Open(destination, componentClock(at))
	if err != nil {
		t.Fatal(err)
	}
	defer componentClose(t, app)
	health, err := app.Health(supervisor, project)
	if err != nil || !health.LastRestore.Equal(at) {
		t.Fatalf("losing restore changed the elected destination's timestamp: %v %v", health.LastRestore, err)
	}
	held, err := app.AssessmentSuspended(supervisor, project)
	if err != nil || !held {
		t.Fatalf("winning destination did not retain its review hold: %t %v", held, err)
	}
	assertComponentHTML(t, app, fixture.KeptReport, fixture.KeptHTML)
	assertComponentImport(t, app, fixture.KeptCSV, 2, fixture.KeptRun)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 3 || entries[0].Name() != "backups" || entries[1].Name() != "original.db" || entries[2].Name() != "restored.db" {
		t.Fatalf("completed attempts left an unexpected file or removed owned evidence: %v %v", entries, err)
	}
	t.Log("overlapping restores published one held destination, preserved the winner's metadata, and removed both attempts' staging")
}

func assertRestoreSourceUnchanged(t *testing.T, backup board.Backup) {
	t.Helper()
	raw, err := os.ReadFile(backup.Path)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != backup.SHA256 {
		t.Fatalf("restore changed its backup source bytes: %v", err)
	}
	sidecar, err := os.ReadFile(backup.Path + ".sha256")
	if err != nil || string(sidecar) != backup.SHA256+"\n" {
		t.Fatalf("restore changed its source sidecar bytes: %v", err)
	}
}
