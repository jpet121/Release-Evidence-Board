package board_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

// AC-004-21, AC-004-22 and AC-005-13. An uncommitted stale selection and an
// exact retry of an already committed stale selection have different outcomes.
func TestFollowupRejectStaleNewConfirmationButRecoverCommittedReceipt(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	candidate := fixtureCandidate(t, app, vectorBaseline())
	const old = "2026-11-17T11:45:00Z"
	const now = "2026-11-17T12:00:00Z"
	oldCSV := csvBytes(t, resultRow(candidate, "T1", "1", "R0-COMPLETE", "OUT0", old, old, "PASS"))
	oldPreview := previewFollowupFile(t, app, candidate.ID, oldCSV)
	oldReceipt, err := app.CommitImport(supervisor, project, oldPreview.Token, oldCSV)
	if err != nil || oldReceipt.LedgerHighWater != 1 {
		t.Fatalf("first valid import did not commit exactly one run: %v", err)
	}
	newCSV := csvBytes(t, resultRow(candidate, "T1", "1", "R2-COMPLETE", "OUT2", now, now, "PASS"))
	stalePreview := previewFollowupFile(t, app, candidate.ID, newCSV)
	if len(stalePreview.Projected) != 1 || stalePreview.Projected[0].Status != "SUFFICIENT" {
		t.Fatal("pre-intervention preview did not observe the complete new run")
	}
	intervening := importBytes(t, app, candidate.ID, csvBytes(t, runOnly(candidate, "R1-INCOMPLETE", now)))
	if intervening.LedgerHighWater != 2 {
		t.Fatal("intervening receipt did not advance the product ledger")
	}
	if rejected, err := app.CommitImport(supervisor, project, stalePreview.Token, newCSV); !errors.Is(err, board.ErrConflict) || rejected.ID != "" {
		t.Fatalf("stale new preview committed or returned the wrong error: %v", err)
	}
	interim, err := app.Assess(supervisor, project, candidate.ID)
	if err != nil || interim.Status != "BLOCKED" || interim.LedgerHighWater != 2 || interim.Behaviors[0].CurrentRunID != "R1-INCOMPLETE" {
		t.Fatalf("stale confirmation changed the intervening assessment: %s, %v", interim.Status, err)
	}
	refreshed := previewFollowupFile(t, app, candidate.ID, newCSV)
	if len(refreshed.Projected) != 1 || refreshed.Projected[0].Status != "BLOCKED" ||
		!refreshed.Projected[0].Behaviors[0].CurrentTieIncomplete || refreshed.Projected[0].Behaviors[0].CurrentRunID != "R1-INCOMPLETE" {
		t.Fatal("refreshed confirmation hid the incomplete equal-finish run")
	}
	newReceipt, err := app.CommitImport(supervisor, project, refreshed.Token, newCSV)
	if err != nil || newReceipt.LedgerHighWater != 3 || !slices.Equal(newReceipt.AddedRuns, []string{"R2-COMPLETE"}) {
		t.Fatalf("refreshed confirmation failed to commit its exact batch: %v", err)
	}
	stored, err := app.SourceCSV(supervisor, project, newReceipt.ID)
	if err != nil || !bytes.Equal(stored, newCSV) {
		t.Fatalf("new receipt source differs from the refreshed preview: %v", err)
	}
	recordApprovalInstance(t, "AC-004-21.P002", map[string]any{"confirmDefect": "stale data revision for uncommitted file"}, "Reject the changed bytes or stale new import. Require a fresh preview")
	before, err := app.Audit(supervisor, project)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := app.CommitImport(supervisor, project, oldPreview.Token, oldCSV)
	if err != nil || replayed.ID != oldReceipt.ID || replayed.LedgerHighWater != 1 {
		t.Fatalf("stale committed preview did not recover the original receipt: %v", err)
	}
	after, err := app.Audit(supervisor, project)
	if err != nil || len(after) != len(before) {
		t.Fatalf("exact committed retry added an audit event: %v", err)
	}
	report, err := app.Freeze(supervisor, project, candidate.ID)
	if err != nil || report.Status != "BLOCKED" || report.LedgerHighWater != 3 {
		t.Fatalf("retry changed the frozen decision or ledger: %s, %v", report.Status, err)
	}
	t.Log("AC-004-21 stale=conflict ledger_after_rejection=2; AC-004-22 source=exact ledger=3; AC-005-13 original_receipt=true audit_growth=0 frozen=BLOCKED")
}

// SPEC-5 J03. Test denied requests from a second contributor who has ordinary
// import permission for this project. The owner then confirms the original CSV.
func TestFollowupForeignPreviewAttemptsPreserveOwnerConfirmation(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	const other = "other-contributor"
	if err := app.SetAccount(supervisor, other, testPassword, []board.Role{board.Contributor}); err != nil {
		t.Fatal(err)
	}
	candidate := fixtureCandidate(t, app, vectorBaseline())
	const now = "2026-11-17T12:00:00Z"
	raw := csvBytes(t, resultRow(candidate, "T1", "1", "OWNER-RUN", "OWNED", now, now, "PASS"))
	staged := previewFollowupFile(t, app, candidate.ID, raw)
	if _, err := app.CommitImport(other, project, staged.Token, raw); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("another contributor confirmed a foreign preview: %v", err)
	}
	if err := app.CancelPreview(other, project, staged.Token); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("another contributor canceled a foreign preview: %v", err)
	}
	changed := csvBytes(t, resultRow(candidate, "T1", "1", "OWNER-RUN", "OWNED", now, now, "FAIL"))
	if _, err := app.CommitImport(supervisor, project, staged.Token, changed); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("owner could replace the previously previewed bytes: %v", err)
	}
	fresh := previewFollowupFile(t, app, candidate.ID, changed)
	if fresh.Token == staged.Token || fresh.Hash == staged.Hash || len(fresh.Projected) != 1 || fresh.Projected[0].Status != "BLOCKED" {
		t.Fatal("changed bytes did not produce their own distinct, confirmable preview")
	}
	if err := app.CancelPreview(supervisor, project, fresh.Token); err != nil {
		t.Fatalf("cancel fresh changed-byte preview: %v", err)
	}
	unchanged, err := app.Assess(supervisor, project, candidate.ID)
	if err != nil || unchanged.Status != "BLOCKED" || unchanged.LedgerHighWater != 0 {
		t.Fatalf("rejected attempts changed unstaged evidence: %s, %v", unchanged.Status, err)
	}
	committed, err := app.CommitImport(supervisor, project, staged.Token, nil)
	if err != nil || committed.Uploader != supervisor || committed.LedgerHighWater != 1 ||
		!slices.Equal(committed.AddedRuns, []string{"OWNER-RUN"}) {
		t.Fatalf("rejected attempts destroyed the owner's preview: %v", err)
	}
	stored, err := app.SourceCSV(supervisor, project, committed.ID)
	if err != nil || !bytes.Equal(stored, raw) {
		t.Fatalf("owner confirmation committed different bytes: %v", err)
	}
	recordApprovalInstance(t, "AC-004-21.P001", map[string]any{"confirmDefect": "changed submitted bytes on valid preview"}, "Reject the changed bytes or stale new import. Require a fresh preview")
	t.Log("AC-004-21 changed_bytes=conflict ledger_after_denials=0; SPEC-5 J03 foreign_confirm=not_found foreign_cancel=not_found; AC-004-22 owner_receipt=true source=exact ledger=1")
}

func previewFollowupFile(t *testing.T, app *board.Board, candidateID string, raw []byte) board.Preview {
	t.Helper()
	preview, err := app.PreviewImport(supervisor, project, candidateID, raw)
	if err != nil || preview.Token == "" || len(preview.Issues) != 0 {
		t.Fatalf("expected usable preview: %v, diagnostics=%v", err, preview.Issues)
	}
	return preview
}
