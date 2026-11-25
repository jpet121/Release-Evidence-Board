package board_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestTicket80PurgeSelectionKeepsUnselectedEvidenceAfterRestart(t *testing.T) {
	now := fixtureTime
	app, path := openBoard(t, func() time.Time { return now })
	first := fixtureCandidate(t, app, vectorBaseline())
	second := newCustodySuccessor(t, app, first, "a")
	stamp := board.FormatUTC(now)
	oldSource := csvBytes(t,
		resultRow(first, "T1", "1", "OLD-A", "RESULT-A", stamp, stamp, "PASS"),
		resultRow(second, "T1", "1", "OLD-B", "RESULT-B", stamp, stamp, "PASS"))
	oldReceipt := importBytes(t, app, first.ID, oldSource)
	oldReport, err := app.Freeze(supervisor, project, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(181 * 24 * time.Hour)
	kept := newCustodySuccessor(t, app, first, "b")
	stamp = board.FormatUTC(now)
	keptSource := csvBytes(t, resultRow(kept, "T1", "1", "KEEP-RUN", "KEEP-RESULT", stamp, stamp, "PASS"))
	keptReceipt := importBytes(t, app, kept.ID, keptSource)
	keptReport, err := app.Freeze(supervisor, project, kept.ID)
	if err != nil || keptReport.Status != board.Sufficient || keptReport.LedgerHighWater != 3 {
		t.Fatalf("fresh independent evidence fixture: %+v, %v", keptReport, err)
	}
	_, keptHTML, err := app.ReportHTML(supervisor, project, keptReport.ID)
	if err != nil {
		t.Fatal(err)
	}
	partial, err := app.PreviewPurge(supervisor, project, []string{first.ID})
	if err != nil || !slices.Equal(partial.SharedBlockers, []string{second.ID}) {
		t.Fatalf("shared source was not protected: %+v, %v", partial, err)
	}
	if _, err := app.Purge(supervisor, project, []string{first.ID}, partial.Confirmation); !errors.Is(err, board.ErrInvalid) {
		t.Fatalf("partial shared purge was accepted: %v", err)
	}
	if raw, err := app.SourceCSV(supervisor, project, oldReceipt.ID); err != nil || !bytes.Equal(raw, oldSource) {
		t.Fatalf("shared purge refusal changed original bytes: %v", err)
	}
	ids := []string{second.ID, first.ID, first.ID}
	plan, err := app.PreviewPurge(supervisor, project, ids)
	if err != nil || len(plan.Ineligible) != 0 || len(plan.SharedBlockers) != 0 || len(plan.CandidateIDs) != 2 || len(plan.Runs) != 2 ||
		!slices.Equal(plan.ReceiptIDs, []string{oldReceipt.ID}) || !slices.Equal(plan.ReportIDs, []string{oldReport.ID}) || len(plan.OrphanBaselines) != 0 {
		t.Fatalf("whole-scope plan changed unrelated ownership: %+v, %v", plan, err)
	}
	wire, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	// Before/after native receipts compare this deterministic public wire value.
	// It is compatibility evidence, not an independently computed status oracle.
	t.Logf("PURGE_WIRE %s", wire)
	removed, err := app.Purge(supervisor, project, ids, plan.Confirmation)
	if err != nil || !reflect.DeepEqual(removed, plan) {
		t.Fatalf("confirmed public plan changed: %+v, %v", removed, err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := board.Open(path, board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	if held, err := reopened.AssessmentSuspended(supervisor, project); err != nil || held {
		t.Fatalf("purge left broken material/run indexes after restart: held=%t, %v", held, err)
	}
	for _, id := range []string{first.ID, second.ID} {
		if _, _, _, err := reopened.Candidate(supervisor, project, id); !errors.Is(err, board.ErrNotFound) {
			t.Fatalf("selected candidate survived: %s, %v", id, err)
		}
	}
	if _, err := reopened.SourceCSV(supervisor, project, oldReceipt.ID); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("selected source survived: %v", err)
	}
	if _, _, err := reopened.ReportHTML(supervisor, project, oldReport.ID); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("selected report survived: %v", err)
	}
	assessment, err := reopened.Assess(supervisor, project, kept.ID)
	if err != nil || assessment.Status != board.Sufficient || assessment.LedgerHighWater != 3 || !assessment.PredecessorGone ||
		len(assessment.Behaviors) != 1 || assessment.Behaviors[0].CurrentRunID != "KEEP-RUN" {
		t.Fatalf("unselected assessment/custody changed: %+v, %v", assessment, err)
	}
	if receipt, err := reopened.Receipt(supervisor, project, keptReceipt.ID); err != nil || !reflect.DeepEqual(receipt, keptReceipt) {
		t.Fatalf("unselected receipt changed: %v", err)
	}
	if raw, err := reopened.SourceCSV(supervisor, project, keptReceipt.ID); err != nil || !bytes.Equal(raw, keptSource) {
		t.Fatalf("unselected source changed: %v", err)
	}
	if report, html, err := reopened.ReportHTML(supervisor, project, keptReport.ID); err != nil || report != keptReport || !bytes.Equal(html, keptHTML) {
		t.Fatalf("unselected frozen report changed: %v", err)
	}
}
