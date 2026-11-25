package board_test

import (
	"bytes"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestAC011And014BackupRestoreAndReplay(t *testing.T) {
	now := fixtureTime
	app, _ := openBoard(t, func() time.Time { return now })
	c := fixtureCandidate(t, app, vectorBaseline())
	stamp := board.FormatUTC(now)
	raw := csvBytes(t, resultRow(c, "T1", "1", "RUN1", "RES1", stamp, stamp, "PASS"))
	receipt := importBytes(t, app, c.ID, raw)
	backupDir := t.TempDir()
	backup, err := app.Backup(supervisor, project, backupDir)
	if err != nil || backup.SHA256 == "" {
		t.Fatalf("backup = %#v, %v", backup, err)
	}

	restoredPath := filepath.Join(t.TempDir(), "clean-store.db")
	opts := board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return now }}
	result, err := board.RestoreBackup(backup.Path, restoredPath, opts)
	if err != nil || !result.IntegrityOK {
		t.Fatalf("restore = %#v, %v", result, err)
	}
	restored, err := board.Open(restoredPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restored.Freeze(supervisor, project, c.ID); !errors.Is(err, board.ErrIntegrity) {
		t.Fatalf("snapshot before revalidation = %v", err)
	}
	if err := restored.Revalidate(supervisor, project); err != nil {
		t.Fatal(err)
	}
	data, err := restored.SourceCSV(supervisor, project, receipt.ID)
	if err != nil || !bytes.Equal(raw, data) {
		t.Fatalf("restored original bytes differ: %v", err)
	}
	assessment, err := restored.Assess(supervisor, project, c.ID)
	if err != nil || assessment.Status != board.Sufficient || assessment.LedgerHighWater != 1 {
		t.Fatalf("assessment after restore = %#v, %v", assessment, err)
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}

	if err := app.RecordRestoreDrill(supervisor, project, restoredPath); err != nil {
		t.Fatal(err)
	}
	now = fixtureTime.Add(26 * time.Hour)
	health, err := app.Health(supervisor, project)
	if err != nil || health.Alert != "" {
		t.Fatalf("healthy backup at exactly 26 hours = %#v, %v", health, err)
	}
	now = now.Add(time.Microsecond)
	health, err = app.Health(supervisor, project)
	if err != nil || !strings.Contains(health.Alert, "more than 26 hours") {
		t.Fatalf("overdue backup alert = %#v, %v", health, err)
	}
	now = fixtureTime.Add(30 * 24 * time.Hour)
	count, err := app.ExpireBackups(supervisor, project, backupDir)
	if err != nil || count != 1 {
		t.Fatalf("backup expiry at 30 days = %d, %v", count, err)
	}
}

func TestAC014SharedCSVAndPurgedPredecessor(t *testing.T) {
	now := fixtureTime
	app, _ := openBoard(t, func() time.Time { return now })
	first := fixtureCandidate(t, app, vectorBaseline())
	next := func(build string) board.Candidate {
		t.Helper()
		input := board.CandidateInput{
			Manifest: vectorManifest(), BaselineDigest: first.BaselineDigest,
			PredecessorID: first.ID, Correction: "New artifact for synthetic recovery",
		}
		input.Manifest.BuildDigest = strings.Repeat(build, 64)
		c, reused, err := app.CreateCandidate(supervisor, project, input)
		if err != nil || reused {
			t.Fatalf("candidate = %#v, %v, %v", c, reused, err)
		}
		return c
	}
	now = now.Add(24 * time.Hour)
	second := next("1")
	at := board.FormatUTC(now)
	receipt := importBytes(t, app, first.ID, csvBytes(t,
		resultRow(first, "T1", "1", "RUN-A", "RES-A", at, at, "PASS"),
		resultRow(second, "T1", "1", "RUN-B", "RES-B", at, at, "PASS")))
	now = fixtureTime.Add(180 * 24 * time.Hour)
	preview, err := app.PreviewPurge(supervisor, project, []string{first.ID})
	if err != nil || !slices.Equal(preview.SharedBlockers, []string{second.ID}) || !preview.ExternalCopiesNotPurged {
		t.Fatalf("shared evidence preview = %#v, %v", preview, err)
	}
	if _, err := app.Purge(supervisor, project, []string{first.ID}, preview.Confirmation); !errors.Is(err, board.ErrInvalid) {
		t.Fatalf("ineligible shared purge = %v", err)
	}
	if _, err := app.SourceCSV(supervisor, project, receipt.ID); err != nil {
		t.Fatal(err)
	}
	third := next("2")
	now = now.Add(24 * time.Hour)
	scope := []string{first.ID, second.ID}
	preview, err = app.PreviewPurge(supervisor, project, scope)
	if err != nil || len(preview.SharedBlockers) != 0 || len(preview.OrphanBaselines) != 0 || len(preview.ReceiptIDs) != 1 {
		t.Fatalf("eligible purge preview = %#v, %v", preview, err)
	}
	removed, err := app.Purge(supervisor, project, scope, preview.Confirmation)
	if err != nil || len(removed.Runs) != 2 {
		t.Fatalf("purge = %#v, %v", removed, err)
	}
	if _, err := app.SourceCSV(supervisor, project, receipt.ID); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("shared CSV after purge = %v", err)
	}
	status, err := app.Assess(supervisor, project, third.ID)
	if err != nil || !status.PredecessorGone || status.Candidate.PredecessorID != first.ID || status.Status != board.Blocked {
		t.Fatalf("retained successor = %#v, %v", status, err)
	}
}

func TestAC01102ConcurrentDraftEdit(t *testing.T) {
	app, _ := openBoard(t, nil)
	draft, err := app.CreateDraft(supervisor, project, vectorBaseline(), board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	type draftEditResult struct {
		text string
		err  error
	}
	results := make(chan draftEditResult, 2)
	for _, text := range []string{"First edited text", "Second edited text"} {
		workers.Go(func() {
			changed := vectorBaseline()
			changed.Behaviors[0].Text = text
			_, err := app.SaveDraft(supervisor, project, draft.ID, draft.Revision, changed)
			results <- draftEditResult{text: text, err: err}
		})
	}
	workers.Wait()
	close(results)
	passed, conflicts := 0, 0
	var winningText string
	for result := range results {
		switch {
		case result.err == nil:
			passed++
			winningText = result.text
		case errors.Is(result.err, board.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected edit error: %v", result.err)
		}
	}
	if passed != 1 || conflicts != 1 {
		t.Fatalf("concurrent edits = %d success, %d conflicts", passed, conflicts)
	}
	current, err := app.Draft(supervisor, project, draft.ID)
	if err != nil || current.Revision != draft.Revision+1 || len(current.Baseline.Behaviors) != 1 || current.Baseline.Behaviors[0].Text != winningText {
		t.Fatalf("concurrent final draft = %#v, %v", current, err)
	}
	recordApprovalInstance(t, "AC-011-02.P001", map[string]any{}, "One succeeds; other conflicts; no overwrite")
}
