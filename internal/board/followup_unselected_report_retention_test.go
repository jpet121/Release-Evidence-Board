package board_test

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupPurgeRetainsUnselectedExpiredReport(t *testing.T) {
	now := fixtureTime
	clock := func() time.Time { return now }
	app, path := openBoard(t, clock)
	type candidateEvidence struct {
		csv     []byte
		receipt board.Receipt
		report  board.Report
		html    []byte
	}
	freeze := func(candidate board.Candidate, run, result string) candidateEvidence {
		t.Helper()
		stamp := board.FormatUTC(now)
		raw := csvBytes(t, resultRow(candidate, "T1", "1", run, result, stamp, stamp, "PASS"))
		receipt := importBytes(t, app, candidate.ID, raw)
		report, err := app.Freeze(supervisor, project, candidate.ID)
		if err != nil || report.Status != board.Sufficient {
			t.Fatalf("could not freeze sufficient candidate %s: %v", candidate.ID, err)
		}
		report, html, err := app.ReportHTML(supervisor, project, report.ID)
		if err != nil || len(html) == 0 {
			t.Fatalf("could not read saved report: %v", err)
		}
		return candidateEvidence{csv: raw, receipt: receipt, report: report, html: html}
	}
	first := fixtureCandidate(t, app, vectorBaseline())
	firstEvidence := freeze(first, "RETENTION-A", "RETENTION-RESULT-A")
	oldAudit, err := app.Audit(supervisor, project)
	if err != nil || len(oldAudit) == 0 {
		t.Fatalf("fixture lacks oldest audit records: %v", err)
	}
	expiringAudit := make([]uint64, 0, len(oldAudit))
	for _, event := range oldAudit {
		expiringAudit = append(expiringAudit, event.Sequence)
	}

	now = now.Add(24 * time.Hour)
	second := newCustodySuccessor(t, app, first, "a")
	secondEvidence := freeze(second, "RETENTION-B", "RETENTION-RESULT-B")
	if firstEvidence.receipt.ID == secondEvidence.receipt.ID {
		t.Fatal("fixture requires independent original CSVs and receipts")
	}
	savedCandidate, savedManifest, savedBaseline, err := app.Candidate(supervisor, project, second.ID)
	if err != nil {
		t.Fatal(err)
	}

	both := []string{first.ID, second.ID}
	now = now.Add(180 * 24 * time.Hour).Add(-time.Microsecond)
	early, err := app.PreviewPurge(supervisor, project, both)
	if err != nil || !slices.Equal(early.Ineligible, []string{second.ID}) {
		t.Fatalf("preview did not identify the newer candidate below the cutoff: %#v, %v", early, err)
	}
	if !slices.Equal(early.Runs, []string{"RETENTION-A"}) {
		t.Errorf("preview included a run from an ineligible candidate: %#v", early.Runs)
	}
	if !slices.Equal(early.AuditSequences, expiringAudit) {
		t.Errorf("preview did not retain the correct audit expiry boundary: got %v, want %v", early.AuditSequences, expiringAudit)
	}
	if _, err := app.Purge(supervisor, project, both, early.Confirmation); !errors.Is(err, board.ErrInvalid) {
		t.Fatalf("confirmation below the cutoff got %v, want ErrInvalid", err)
	}

	now = now.Add(time.Microsecond)
	full, err := app.PreviewPurge(supervisor, project, both)
	if err != nil || len(full.Ineligible) != 0 || len(full.SharedBlockers) != 0 {
		t.Fatalf("both independently recorded candidates should now be eligible: %#v, %v", full, err)
	}
	selected := []string{first.ID}
	if _, err := app.Purge(supervisor, project, selected, full.Confirmation); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("confirmation for a different eligible scope got %v, want ErrConflict", err)
	}
	plan, err := app.PreviewPurge(supervisor, project, selected)
	if err != nil || len(plan.Ineligible) != 0 || len(plan.SharedBlockers) != 0 ||
		!slices.Equal(plan.CandidateIDs, selected) || !slices.Equal(plan.ReceiptIDs, []string{firstEvidence.receipt.ID}) ||
		!slices.Equal(plan.Runs, []string{"RETENTION-A"}) || len(plan.OrphanBaselines) != 0 {
		t.Fatalf("preview did not isolate the selected candidate, receipt, run and retained descriptor: %#v, %v", plan, err)
	}
	if !slices.Equal(plan.ReportIDs, []string{firstEvidence.report.ID}) {
		t.Errorf("preview included a report from outside the selected purge scope: %v", plan.ReportIDs)
	}
	purged, err := app.Purge(supervisor, project, selected, plan.Confirmation)
	if err != nil || !reflect.DeepEqual(purged, plan) {
		t.Fatalf("purge did not commit its fresh confirmed scope: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	app, err = board.Open(path, board.Settings{ProjectID: project, ProductID: "DEMO", Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})

	if _, _, _, err := app.Candidate(supervisor, project, first.ID); !errors.Is(err, board.ErrNotFound) {
		t.Errorf("selected candidate still exists after reopening: %v", err)
	}
	if _, _, err := app.ReportHTML(supervisor, project, firstEvidence.report.ID); !errors.Is(err, board.ErrNotFound) {
		t.Errorf("selected frozen report still exists after reopening: %v", err)
	}
	if _, err := app.Receipt(supervisor, project, firstEvidence.receipt.ID); !errors.Is(err, board.ErrNotFound) {
		t.Errorf("selected receipt still exists after reopening: %v", err)
	}
	if _, err := app.SourceCSV(supervisor, project, firstEvidence.receipt.ID); !errors.Is(err, board.ErrNotFound) {
		t.Errorf("selected original CSV still exists after reopening: %v", err)
	}
	candidate, manifest, baseline, err := app.Candidate(supervisor, project, second.ID)
	if err != nil || !reflect.DeepEqual(candidate, savedCandidate) || !reflect.DeepEqual(manifest, savedManifest) ||
		!reflect.DeepEqual(baseline, savedBaseline) {
		t.Errorf("unselected candidate or its descriptors changed after reopening: %v", err)
	}
	report, html, err := app.ReportHTML(supervisor, project, secondEvidence.report.ID)
	if err != nil || !reflect.DeepEqual(report, secondEvidence.report) || !bytes.Equal(html, secondEvidence.html) {
		t.Errorf("unselected frozen report metadata or bytes changed after reopening: %v", err)
	}
	receipt, err := app.Receipt(supervisor, project, secondEvidence.receipt.ID)
	if err != nil || !reflect.DeepEqual(receipt, secondEvidence.receipt) {
		t.Errorf("unselected receipt changed after reopening: %v", err)
	}
	raw, err := app.SourceCSV(supervisor, project, secondEvidence.receipt.ID)
	if err != nil || !bytes.Equal(raw, secondEvidence.csv) {
		t.Errorf("unselected original CSV changed after reopening: %v", err)
	}
	events, err := app.Audit(supervisor, project)
	if err != nil || !slices.ContainsFunc(events, func(event board.AuditEvent) bool {
		return event.Actor == supervisor && event.Action == "retention.purge" && event.Outcome == "success" &&
			slices.Contains(event.References, first.ID) && !slices.Contains(event.References, second.ID) &&
			slices.Contains(event.References, "reports=1") && slices.Contains(event.References, "receipts=1")
	}) {
		t.Errorf("reopened purge audit lacked the selected scope and exact counts: %v", err)
	}

	last, err := app.PreviewPurge(supervisor, project, []string{second.ID})
	if err != nil || len(last.Ineligible) != 0 || len(last.SharedBlockers) != 0 {
		t.Fatalf("fresh scope did not admit the remaining eligible candidate: %#v, %v", last, err)
	}
	if !slices.Equal(last.OrphanBaselines, []string{second.BaselineDigest}) {
		t.Errorf("last eligible scope did not include its orphaned approved descriptor: %v", last.OrphanBaselines)
	}
	if _, err := app.Purge(supervisor, project, []string{second.ID}, last.Confirmation); err != nil {
		t.Fatalf("purge of the formerly unselected candidate now in scope failed: %v", err)
	}
	if _, _, err := app.ReportHTML(supervisor, project, secondEvidence.report.ID); !errors.Is(err, board.ErrNotFound) {
		t.Errorf("fresh authorized purge did not remove the remaining report: %v", err)
	}
}
