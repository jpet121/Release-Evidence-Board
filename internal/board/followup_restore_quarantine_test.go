package board_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"release-evidence-board/internal/board"
)

func TestFollowupRestoreQuarantineSurvivesRecovery(t *testing.T) {
	fixture := newBackupCustodyFixture(t)
	t.Cleanup(func() { assertRestoreSourceUnchanged(t, fixture.original) })
	original, err := os.ReadFile(fixture.original.Path)
	if err != nil || custodyDigest(original) != fixture.original.SHA256 {
		t.Fatalf("independently preserved backup does not match its original hash: %v", err)
	}
	csv, err := fixture.app.SourceCSV(supervisor, project, fixture.receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, defect := range []struct {
		name    string
		sidecar []byte
	}{
		{"missing-checksum", nil},
		{"mismatched-checksum", []byte(strings.Repeat("0", 64) + "\n")},
	} {
		for _, sequence := range []struct {
			name  string
			steps []string
		}{
			{"suspension", []string{"suspend"}},
			{"derived-restore", []string{"backup-restore"}},
			{"repeated-operations", []string{"suspend", "backup-restore", "suspend", "backup-restore"}},
		} {
			t.Run(defect.name+"/"+sequence.name, func(t *testing.T) {
				dir := t.TempDir()
				source := filepath.Join(dir, "unverified.db")
				if err := os.WriteFile(source, original, 0600); err != nil {
					t.Fatal(err)
				}
				if defect.sidecar != nil {
					if err := os.WriteFile(source+".sha256", defect.sidecar, 0600); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() {
					copied, err := os.ReadFile(source)
					if err != nil || !bytes.Equal(copied, original) {
						t.Fatalf("recovery changed unverified source bytes: %v", err)
					}
					actual, err := os.ReadFile(source + ".sha256")
					if defect.sidecar == nil && !errors.Is(err, os.ErrNotExist) ||
						defect.sidecar != nil && (err != nil || !bytes.Equal(actual, defect.sidecar)) {
						t.Fatalf("recovery changed the unverified source checksum state: %v", err)
					}
				})
				path := filepath.Join(dir, "initial.db")
				verification := restoreQuarantineFixture(t, source, path)
				assertRestoreQuarantine(t, path, "initial restore", verification.IntegrityOK, fixture, csv)
				for i, step := range sequence.steps {
					app := openQuarantineFixture(t, path)
					if step == "suspend" {
						err := app.SuspendAssessments(supervisor, project)
						componentClose(t, app)
						if err != nil {
							t.Fatal(err)
						}
					} else {
						derived, err := app.Backup(supervisor, project, filepath.Join(dir, fmt.Sprintf("backup-%d", i)))
						componentClose(t, app)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { assertRestoreSourceUnchanged(t, derived) })
						path = filepath.Join(dir, fmt.Sprintf("derived-%d.db", i))
						verification = restoreQuarantineFixture(t, derived.Path, path)
					}
					assertRestoreQuarantine(t, path, step, verification.IntegrityOK, fixture, csv)
				}
			})
		}
	}

	t.Run("independently-verified-recovery", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "verified.db")
		verified := restoreQuarantineFixture(t, fixture.original.Path, path)
		if !verified.IntegrityOK {
			t.Fatal("independently verified backup did not restore")
		}
		app := openQuarantineFixture(t, path)
		defer componentClose(t, app)
		reviewer := authenticateRestoreReviewer(t, app)
		held, holdErr := app.AssessmentSuspended(reviewer, project)
		_, assessErr := app.Assess(reviewer, project, fixture.candidate.ID)
		_, freezeErr := app.Freeze(reviewer, project, fixture.candidate.ID)
		if holdErr != nil || !held || !errors.Is(assessErr, board.ErrIntegrity) || !errors.Is(freezeErr, board.ErrIntegrity) {
			t.Fatalf("verified restore did not wait for reviewer: held=%t, hold=%v, assess=%v, freeze=%v", held, holdErr, assessErr, freezeErr)
		}
		if err := app.Revalidate(reviewer, project); err != nil {
			t.Fatalf("authenticated reviewer could not revalidate independently verified recovery: %v", err)
		}
		assessment, err := app.Assess(reviewer, project, fixture.candidate.ID)
		if err != nil || assessment.Status != board.Sufficient || assessment.LedgerHighWater != fixture.receipt.LedgerHighWater {
			t.Fatalf("verified recovery lost sufficient evidence or its ledger: %+v; %v", assessment, err)
		}
		report, err := app.Freeze(reviewer, project, fixture.candidate.ID)
		if err != nil || report.Status != board.Sufficient || report.ID == fixture.report.ID ||
			report.LedgerHighWater != fixture.receipt.LedgerHighWater {
			t.Fatalf("verified recovery could not issue an independent new report: %+v; %v", report, err)
		}
		_, html, err := app.ReportHTML(reviewer, project, report.ID)
		if err != nil || custodyDigest(html) != report.SHA256 || !bytes.Contains(html, []byte("CUSTODY-RESULT")) {
			t.Fatalf("new verified report could not reproduce the original evidence: %v", err)
		}
		assertRestoreQuarantineEvidence(t, app, fixture, csv)
		t.Logf("independent verified recovery revalidated and froze %s %s at original ledger %d", report.ID, report.Status, report.LedgerHighWater)
	})
}

func restoreQuarantineFixture(t *testing.T, source, destination string) board.RestoreResult {
	t.Helper()
	result, err := board.RestoreBackup(source, destination, componentClock(fixtureTime))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func openQuarantineFixture(t *testing.T, path string) *board.Board {
	t.Helper()
	app, err := board.Open(path, componentClock(fixtureTime))
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func authenticateRestoreReviewer(t *testing.T, app *board.Board) string {
	t.Helper()
	reviewer, err := app.Authenticate(supervisor, testPassword)
	if err != nil || !reviewer.CanReview() {
		t.Fatalf("restore reviewer could not authenticate: %v", err)
	}
	return reviewer.ID
}

func assertRestoreQuarantine(t *testing.T, path, operation string, verified bool, fixture backupCustodyFixture, csv []byte) {
	t.Helper()
	app := openQuarantineFixture(t, path)
	defer componentClose(t, app)
	reviewer := authenticateRestoreReviewer(t, app)
	revalidation := app.Revalidate(reviewer, project)
	held, holdErr := app.AssessmentSuspended(reviewer, project)
	assessment, assessErr := app.Assess(reviewer, project, fixture.candidate.ID)
	report, freezeErr := app.Freeze(reviewer, project, fixture.candidate.ID)
	assertRestoreQuarantineEvidence(t, app, fixture, csv)
	if verified || holdErr != nil || !held || !errors.Is(revalidation, board.ErrIntegrity) ||
		!errors.Is(assessErr, board.ErrIntegrity) || !errors.Is(freezeErr, board.ErrIntegrity) ||
		assessment.Status != "" || report.ID != "" || report.Status != "" {
		t.Fatalf("quarantine lost after %s: verified=%t, held=%t, hold_err=%v, revalidate_err=%v, assessment=%q, assess_err=%v, new_report=%q, report_status=%q, ledger=%d, freeze_err=%v",
			operation, verified, held, holdErr, revalidation, assessment.Status, assessErr, report.ID, report.Status, report.LedgerHighWater, freezeErr)
	}
	t.Logf("quarantine preserved after %s: verification remains false, live assessment and export refused, historical evidence and ledger retained", operation)
}

func assertRestoreQuarantineEvidence(t *testing.T, app *board.Board, fixture backupCustodyFixture, csv []byte) {
	t.Helper()
	receipt, err := app.Receipt(supervisor, project, fixture.receipt.ID)
	if err != nil || !reflect.DeepEqual(receipt, fixture.receipt) {
		t.Fatalf("recovery changed the committed receipt or ledger: %v", err)
	}
	raw, err := app.SourceCSV(supervisor, project, fixture.receipt.ID)
	if err != nil || !bytes.Equal(raw, csv) || custodyDigest(raw) != receipt.Hash {
		t.Fatalf("recovery changed original CSV bytes: %v", err)
	}
	assertComponentHTML(t, app, fixture.report, fixture.html)
}
