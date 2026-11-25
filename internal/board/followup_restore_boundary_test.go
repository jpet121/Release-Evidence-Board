package board_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupHealthyRestoreNeedsDistinctReviewerBeforeFreeze(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	const operator, reviewer = "restore-operator", "restore-reviewer"
	for account, roles := range map[string][]board.Role{operator: {board.Operator}, reviewer: {board.Reviewer}} {
		if err := app.SetAccount(supervisor, account, testPassword, roles); err != nil {
			t.Fatal(err)
		}
	}
	candidate := fixtureCandidate(t, app, vectorBaseline())
	stamp := board.FormatUTC(fixtureTime)
	raw := csvBytes(t, resultRow(candidate, "T1", "1", "RECOVERY-RUN", "RECOVERY-RESULT", stamp, stamp, "PASS"))
	receipt := importBytes(t, app, candidate.ID, raw)
	backup, err := app.Backup(operator, project, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "clean-recovered-store.db")
	opts := board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return fixtureTime }}
	report, err := board.RestoreBackup(backup.Path, path, opts)
	if err != nil || !report.IntegrityOK {
		t.Fatalf("healthy source failed physical restore: %v", err)
	}
	restored, err := board.Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restored.Close(); err != nil {
			t.Error(err)
		}
	})
	held, err := restored.AssessmentSuspended(reviewer, project)
	if err != nil || !held {
		t.Fatalf("physical restore implicitly revalidated a release decision: %v", err)
	}
	if _, err := restored.Freeze(reviewer, project, candidate.ID); !errors.Is(err, board.ErrIntegrity) {
		t.Fatalf("reviewer froze new evidence while restored store was held: %v", err)
	}
	if err := restored.Revalidate(operator, project); !errors.Is(err, board.ErrForbidden) {
		t.Fatalf("operator-only account revalidated a release decision: %v", err)
	}
	if _, err := restored.Freeze(reviewer, project, candidate.ID); !errors.Is(err, board.ErrIntegrity) {
		t.Fatalf("refused operator activity lifted the hold: %v", err)
	}
	if err := restored.Revalidate(reviewer, project); err != nil {
		t.Fatalf("distinct reviewer could not verify the intact restore: %v", err)
	}

	actual, err := restored.Receipt(reviewer, project, receipt.ID)
	if err != nil || !reflect.DeepEqual(actual, receipt) {
		t.Fatalf("restored receipt no longer binds the same source and ledger: %v", err)
	}
	stored, err := restored.SourceCSV(reviewer, project, receipt.ID)
	if err != nil || !bytes.Equal(raw, stored) {
		t.Fatalf("restored exact source bytes differ: %v", err)
	}
	frozen, err := restored.Freeze(reviewer, project, candidate.ID)
	if err != nil || frozen.Status != board.Sufficient || frozen.LedgerHighWater != receipt.LedgerHighWater {
		t.Fatalf("reviewer-verified restore could not produce a fresh sufficient report: %v", err)
	}
	_, html, err := restored.ReportHTML(reviewer, project, frozen.ID)
	if err != nil || !bytes.Contains(html, []byte("RECOVERY-RESULT")) {
		t.Fatalf("fresh released report omitted restored evidence: %v", err)
	}
	recordApprovalInstance(t, "AC-011-05.P001", map[string]any{}, "Blocked until integrity and reviewer steps complete")
}

func TestFollowupBackupExpiresAtThirtyDaysWithAudit(t *testing.T) {
	now := fixtureTime
	app, _ := openBoard(t, func() time.Time { return now })
	directory := t.TempDir()
	old, err := app.Backup(supervisor, project, directory)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	recent, err := app.Backup(supervisor, project, directory)
	if err != nil {
		t.Fatal(err)
	}
	now = fixtureTime.Add(30 * 24 * time.Hour).Add(-time.Microsecond)
	count, err := app.ExpireBackups(supervisor, project, directory)
	if err != nil || count != 0 {
		t.Fatalf("backup expired one microsecond too early: %d, %v", count, err)
	}
	now = now.Add(time.Microsecond)
	count, err = app.ExpireBackups(supervisor, project, directory)
	if err != nil || count != 1 {
		t.Fatalf("expiry at 30 days removed %d backups: %v", count, err)
	}
	for _, path := range []string{old.Path, old.Path + ".sha256"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired backup content remained at %s: %v", filepath.Base(path), err)
		}
	}
	for _, path := range []string{recent.Path, recent.Path + ".sha256"} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expiry removed a younger backup: %v", err)
		}
	}
	events, err := app.Audit(supervisor, project)
	if err != nil || !slices.ContainsFunc(events, func(event board.AuditEvent) bool {
		return event.Actor == supervisor && event.Action == "backup.expire" && event.Outcome == "success" &&
			event.Time.Equal(now) && slices.Equal(event.References, []string{filepath.Base(old.Path)})
	}) {
		t.Fatalf("backup removal was not attributed and logged: %v", err)
	}
	recordApprovalInstance(t, "AC-014-08.P001", map[string]any{"backupAgeDays": 30}, "Expired backup removed; operation logged")
}

func TestFollowupMonthlyRestoreDrillRecordedAndEventuallyDue(t *testing.T) {
	now := fixtureTime
	app, _ := openBoard(t, func() time.Time { return now })
	store := t.TempDir()
	backup, err := app.Backup(supervisor, project, store)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "monthly-restore.db")
	options := board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return now }}
	restored, err := board.RestoreBackup(backup.Path, destination, options)
	if err != nil || !restored.IntegrityOK {
		t.Fatalf("monthly fixture had no valid restore: %v", err)
	}
	if err := app.RecordRestoreDrill(supervisor, project, destination); err != nil {
		t.Fatal(err)
	}
	current, err := app.Health(supervisor, project)
	if err != nil || !current.LastDrill.Equal(fixtureTime) || current.Alert != "" {
		t.Fatalf("valid current drill missing from actual health records: %#v %v", current, err)
	}
	recordApprovalInstance(t, "AC-014-11.P001", map[string]any{"drillSchedule": "latest monthly drill present"},
		"Latest required drill present or gate/health record shows overdue")

	now = fixtureTime.AddDate(0, 1, 0).Add(time.Microsecond)
	if _, err := app.Backup(supervisor, project, store); err != nil {
		t.Fatal(err)
	}
	overdue, err := app.Health(supervisor, project)
	if err != nil || !overdue.LastDrill.Equal(fixtureTime) || !overdue.LastBackup.Equal(now) ||
		!strings.Contains(overdue.Alert, "monthly restore drill is due") {
		t.Fatalf("fresh backup obscured the overdue independent drill: %#v %v", overdue, err)
	}
	recordApprovalInstance(t, "AC-014-11.P002", map[string]any{"drillSchedule": "monthly drill overdue"},
		"Latest required drill present or gate/health record shows overdue")
}
