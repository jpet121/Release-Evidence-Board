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

const (
	componentInterruptionDir   = "REB_COMPONENT_INTERRUPTION_DIR"
	componentInterruptionTrial = "REB_COMPONENT_INTERRUPTION_TRIAL"
)

type interruptionFixture struct {
	Old, Kept               string
	OldRun, KeptRun         string
	OldReceipt, KeptReceipt string
	OldCSV, KeptCSV         []byte
	OldReport, KeptReport   board.Report
	OldHTML, KeptHTML       []byte
	Backup                  board.Backup
}

type interruptionTrial struct {
	ID, Component, Phase, Action string
	ClockCall, Exit              int
}

var componentTrials = []interruptionTrial{
	{"snapshot-interrupted", "snapshot generation", "report-written-before-audit-commit", "report.freeze", 2, 81},
	{"snapshot-committed", "snapshot generation", "report-and-audit-returned", "report.freeze", 0, 82},
	{"backup-interrupted", "backup and restore", "checked-backup-pair-published-before-audit-commit", "backup.create", 2, 81},
	{"backup-committed", "backup and restore", "backup-pair-and-audit-returned", "backup.create", 0, 82},
	{"restore-interrupted", "backup and restore", "restored-hold-written-before-audit-commit", "backup.restore", 3, 81},
	{"restore-committed", "backup and restore", "restored-hold-and-audit-returned", "backup.restore", 0, 82},
	{"purge-interrupted", "purge journal", "purge-and-tombstones-written-before-audit-commit", "retention.purge", 2, 81},
	{"purge-committed", "purge journal", "purge-and-audit-returned", "retention.purge", 0, 82},
}

func componentClock(at time.Time) board.Settings {
	return board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return at }}
}

func componentClose(t *testing.T, app *board.Board) {
	t.Helper()
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
}

func createInterruptionFixture(t *testing.T, dir string) interruptionFixture {
	t.Helper()
	now := fixtureTime
	app, err := board.Open(filepath.Join(dir, "original.db"), board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer componentClose(t, app)
	if err := app.Bootstrap(supervisor, testPassword); err != nil {
		t.Fatal(err)
	}
	old := fixtureCandidate(t, app, vectorBaseline())
	oldCSV := csvBytes(t, resultRow(old, "T1", "1", "INTERRUPT-OLD", "INTERRUPT-OLD-R", board.FormatUTC(now), board.FormatUTC(now), "PASS"))
	oldReceipt := importBytes(t, app, old.ID, oldCSV)
	oldReport, oldHTML := freezeInterruptionFixture(t, app, old.ID)
	now = now.Add(time.Hour)
	kept := newCustodySuccessor(t, app, old, "a")
	keptCSV := csvBytes(t, resultRow(kept, "T1", "1", "INTERRUPT-KEPT", "INTERRUPT-KEPT-R", board.FormatUTC(now), board.FormatUTC(now), "PASS"))
	keptReceipt := importBytes(t, app, kept.ID, keptCSV)
	keptReport, keptHTML := freezeInterruptionFixture(t, app, kept.ID)
	backup, err := app.Backup(supervisor, project, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	return interruptionFixture{old.ID, kept.ID, "INTERRUPT-OLD", "INTERRUPT-KEPT", oldReceipt.ID, keptReceipt.ID,
		oldCSV, keptCSV, oldReport, keptReport, oldHTML, keptHTML, backup}
}

func freezeInterruptionFixture(t *testing.T, app *board.Board, candidate string) (board.Report, []byte) {
	t.Helper()
	report, err := app.Freeze(supervisor, project, candidate)
	if err != nil || report.Status != board.Sufficient {
		t.Fatalf("freeze positive control: %+v, %v", report, err)
	}
	_, html, err := app.ReportHTML(supervisor, project, report.ID)
	if err != nil || len(html) == 0 || fmt.Sprintf("%x", sha256.Sum256(html)) != report.SHA256 {
		t.Fatalf("snapshot control lost protected bytes: %v", err)
	}
	return report, html
}

func TestFollowupNativeComponentInterruption(t *testing.T) {
	for _, trial := range componentTrials {
		t.Run(trial.ID, func(t *testing.T) {
			dir := t.TempDir()
			fixture := createInterruptionFixture(t, dir)
			encoded, err := json.Marshal(fixture)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "identity.json"), encoded, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := crashChildContext(t)
			defer cancel()
			output, err := runCrashTestChild(ctx, "TestFollowupNativeComponentChild", componentInterruptionDir+"="+dir, componentInterruptionTrial+"="+trial.ID)
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != trial.Exit {
				t.Fatalf("owned child did not reach exact bounded exit %d: %v; %s", trial.Exit, err, output)
			}
			if !bytes.Contains(output, []byte("REB_NATIVE_PHASE "+trial.Phase+"\n")) {
				t.Fatalf("owned child did not observe phase %s: %s", trial.Phase, output)
			}
			observeComponentRestart(t, dir, trial, fixture)
		})
	}
}

func TestFollowupNativeComponentChild(t *testing.T) {
	dir := os.Getenv(componentInterruptionDir)
	if dir == "" {
		return
	}
	index := slices.IndexFunc(componentTrials, func(trial interruptionTrial) bool { return trial.ID == os.Getenv(componentInterruptionTrial) })
	if index < 0 {
		t.Fatal("invalid component interruption trial")
	}
	trial := componentTrials[index]
	encoded, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture interruptionFixture
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatal(err)
	}
	now := fixtureTime.Add(2 * time.Hour)
	if trial.Component == "purge journal" {
		now = fixtureTime.Add(180 * 24 * time.Hour)
	}
	armed, calls := false, 0
	clock := func() time.Time {
		if armed {
			calls++
			if trial.ClockCall != 0 && calls == trial.ClockCall {
				fmt.Println("REB_NATIVE_PHASE " + trial.Phase)
				os.Exit(trial.Exit)
			}
		}
		return now
	}
	settings := board.Settings{ProjectID: project, ProductID: "DEMO", Clock: clock}
	if trial.Action == "backup.restore" {
		armed = true
		result, err := board.RestoreBackup(fixture.Backup.Path, filepath.Join(dir, "restored.db"), settings)
		if err != nil || !result.IntegrityOK {
			t.Fatalf("restore child failed before expected phase: %v", err)
		}
	} else {
		app, err := board.Open(filepath.Join(dir, "original.db"), settings)
		if err != nil {
			t.Fatal(err)
		}
		var plan board.PurgePlan
		if trial.Action == "retention.purge" {
			plan, err = app.PreviewPurge(supervisor, project, []string{fixture.Old})
			if err != nil || len(plan.Ineligible) != 0 || len(plan.SharedBlockers) != 0 || len(plan.ReceiptIDs) != 1 || len(plan.ReportIDs) != 1 {
				t.Fatalf("unqualified destructive test fixture: %#v, %v", plan, err)
			}
		}
		armed = true
		switch trial.Action {
		case "report.freeze":
			_, err = app.Freeze(supervisor, project, fixture.Kept)
		case "backup.create":
			_, err = app.Backup(supervisor, project, filepath.Join(dir, "backups"))
		case "retention.purge":
			_, err = app.Purge(supervisor, project, []string{fixture.Old}, plan.Confirmation)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if trial.ClockCall != 0 {
		t.Fatal("operation returned before the targeted interruption")
	}
	fmt.Println("REB_NATIVE_PHASE " + trial.Phase)
	os.Exit(trial.Exit)
}

func observeComponentRestart(t *testing.T, dir string, trial interruptionTrial, fixture interruptionFixture) {
	t.Helper()
	path := filepath.Join(dir, "original.db")
	restoreUnpublished := false
	if trial.Action == "backup.restore" {
		destination := filepath.Join(dir, "restored.db")
		_, err := os.Lstat(destination)
		restoreUnpublished = trial.ClockCall != 0 && errors.Is(err, os.ErrNotExist)
		if !restoreUnpublished {
			if err != nil {
				t.Fatalf("restore did not publish its expected held destination: %v", err)
			}
			path = destination
		}
	}
	settings := componentClock(fixtureTime.Add(2 * time.Hour))
	app, err := board.Open(path, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer componentClose(t, app)
	committed := trial.ClockCall == 0
	if trial.Action == "backup.restore" && !restoreUnpublished {
		held, err := app.AssessmentSuspended(supervisor, project)
		if err != nil || !held {
			t.Fatalf("interrupted or committed restore exposed usable assessment before reviewer revalidation: held=%t, %v", held, err)
		}
	}
	assertComponentImport(t, app, fixture.KeptCSV, 2, fixture.KeptRun)
	assertComponentHTML(t, app, fixture.KeptReport, fixture.KeptHTML)
	oldAbsent := trial.Action == "retention.purge" && committed
	if oldAbsent {
		_, _, _, candidate := app.Candidate(supervisor, project, fixture.Old)
		_, receipt := app.Receipt(supervisor, project, fixture.OldReceipt)
		_, original := app.SourceCSV(supervisor, project, fixture.OldReceipt)
		_, _, report := app.ReportHTML(supervisor, project, fixture.OldReport.ID)
		for _, err := range []error{candidate, receipt, original, report} {
			if !errors.Is(err, board.ErrNotFound) {
				t.Fatalf("committed purge retained a scoped record: %v", err)
			}
		}
	} else {
		assertComponentImport(t, app, fixture.OldCSV, 1, fixture.OldRun)
		assertComponentHTML(t, app, fixture.OldReport, fixture.OldHTML)
	}
	events := intakeAudit(t, app)
	wantAudit := map[string]int{"report.freeze": 2, "backup.create": 1, "backup.restore": 0, "retention.purge": 0}[trial.Action]
	if oldAbsent {
		wantAudit = 1
	} else if committed {
		wantAudit++
	}
	count := 0
	for _, event := range events {
		if event.Action == trial.Action && event.Outcome == "success" {
			count++
		}
	}
	if count != wantAudit {
		t.Fatalf("unexpected durable count for %s: got %d, want %d", trial.Action, count, wantAudit)
	}
	if trial.Action == "report.freeze" {
		reports, err := app.Reports(supervisor, project, fixture.Kept)
		if err != nil || len(reports) != 1+count-2 {
			t.Fatalf("snapshot metadata did not reconcile with durable audit: %v", err)
		}
		for _, report := range reports {
			_, html, err := app.ReportHTML(supervisor, project, report.ID)
			if err != nil || fmt.Sprintf("%x", sha256.Sum256(html)) != report.SHA256 || report.LedgerHighWater != 2 {
				t.Fatalf("snapshot publication changed protected HTML or revision: %v", err)
			}
		}
	}
	if trial.Action == "backup.create" {
		copies, err := filepath.Glob(filepath.Join(dir, "backups", "reb-backup-*.db"))
		if err != nil || len(copies) != 2 {
			t.Fatalf("expected two complete backup pairs at the exact audit boundary: %v", err)
		}
		for index, copy := range copies {
			if copy == fixture.Backup.Path {
				original, err := os.ReadFile(copy)
				if err != nil || fmt.Sprintf("%x", sha256.Sum256(original)) != fixture.Backup.SHA256 {
					t.Fatalf("interruption altered the earlier backup bytes: %v", err)
				}
			}
			drill := filepath.Join(dir, fmt.Sprintf("drill-%d.db", index))
			result, err := board.RestoreBackup(copy, drill, settings)
			if err != nil || !result.IntegrityOK {
				t.Fatalf("published backup pair is not readable by real restore: %v", err)
			}
			copyApp, err := board.Open(drill, settings)
			if err != nil {
				t.Fatal(err)
			}
			assertComponentImport(t, copyApp, fixture.KeptCSV, 2, fixture.KeptRun)
			assertComponentHTML(t, copyApp, fixture.KeptReport, fixture.KeptHTML)
			componentClose(t, copyApp)
		}
	}
	t.Logf("REB_COMPONENT_INTERRUPTION %s", interruptionObservation(trial, fixture, count))
}

func assertComponentHTML(t *testing.T, app *board.Board, expected board.Report, bytesBefore []byte) {
	t.Helper()
	stored, html, err := app.ReportHTML(supervisor, project, expected.ID)
	if err != nil || stored.ID != expected.ID || stored.SHA256 != expected.SHA256 || stored.LedgerHighWater != expected.LedgerHighWater || !bytes.Equal(html, bytesBefore) {
		t.Fatalf("native restart changed frozen identity, bytes or ledger: %v", err)
	}
}

func assertComponentImport(t *testing.T, app *board.Board, raw []byte, sequence uint64, run string) {
	t.Helper()
	id := fmt.Sprintf("I-%x", sha256.Sum256(raw))
	receipt, err := app.Receipt(supervisor, project, id)
	if err != nil || receipt.Rows != 1 || receipt.LedgerHighWater != sequence || !slices.Equal(receipt.AddedRuns, []string{run}) {
		t.Fatalf("native restart changed the retained receipt or row count: %v", err)
	}
	stored, err := app.SourceCSV(supervisor, project, id)
	if err != nil || !bytes.Equal(stored, raw) {
		t.Fatalf("native restart changed the original producer bytes: %v", err)
	}
	if !slices.ContainsFunc(intakeAudit(t, app), func(event board.AuditEvent) bool {
		return event.Action == "import.commit" && event.Outcome == "success" && event.Actor == supervisor && slices.Contains(event.References, id)
	}) {
		t.Fatal("native restart lost the attributed committed import")
	}
}

func interruptionObservation(trial interruptionTrial, fixture interruptionFixture, audit int) string {
	result := map[string]any{"id": trial.ID, "component": trial.Component, "phase": trial.Phase, "child_exit": trial.Exit,
		"original_sha256": fmt.Sprintf("%x", sha256.Sum256(fixture.KeptCSV)), "snapshot_sha256": fixture.KeptReport.SHA256,
		"receipt_id": fixture.KeptReceipt, "ledger": fixture.KeptReport.LedgerHighWater, "audit_success_count": audit}
	encoded, err := json.Marshal(result)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}
