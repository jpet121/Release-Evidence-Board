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

func TestFollowupPurgeRespectsExactAgeSharedDescriptorAndAudit(t *testing.T) {
	now := fixtureTime
	app, _ := openBoard(t, func() time.Time { return now })
	old := fixtureCandidate(t, app, vectorBaseline())
	stamp := board.FormatUTC(now)
	raw := csvBytes(t, resultRow(old, "T1", "1", "EXPIRING-RUN", "EXPIRING-RESULT", stamp, stamp, "PASS"))
	receipt := importBytes(t, app, old.ID, raw)
	frozen, err := app.Freeze(supervisor, project, old.ID)
	if err != nil || frozen.Status != board.Sufficient {
		t.Fatalf("cannot export the real expiring snapshot: %v", err)
	}
	_, html, err := app.ReportHTML(supervisor, project, frozen.ID)
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "independent-copy.html")
	if err := os.WriteFile(external, html, 0600); err != nil {
		t.Fatal(err)
	}

	now = now.Add(24 * time.Hour)
	retained := newCustodySuccessor(t, app, old, "a")
	_, _, descriptor, err := app.Candidate(supervisor, project, retained.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := app.Audit(supervisor, project)
	if err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(events, func(event board.AuditEvent) bool {
		return event.Action == "candidate.create" && slices.Contains(event.References, retained.ID)
	})
	if index < 0 {
		t.Fatal("fixture lacks the young candidate's audit event")
	}
	young := events[index]

	t.Run("AC-014-03.P001", func(t *testing.T) {
		now = fixtureTime.Add(180 * 24 * time.Hour).Add(-time.Microsecond)
		plan, err := app.PreviewPurge(supervisor, project, []string{old.ID})
		if err != nil || !slices.Equal(plan.Ineligible, []string{old.ID}) {
			t.Fatalf("candidate one microsecond before expiry was marked eligible: %v", err)
		}
		if _, err := app.Purge(supervisor, project, []string{old.ID}, plan.Confirmation); !errors.Is(err, board.ErrInvalid) {
			t.Fatalf("confirming an early purge returned %v", err)
		}
		stored, err := app.SourceCSV(supervisor, project, receipt.ID)
		if err != nil || !bytes.Equal(stored, raw) {
			t.Fatalf("early purge changed committed evidence: %v", err)
		}
		recordApprovalInstance(t, "AC-014-03.P001", map[string]any{"candidateAge": "179 days 23:59:59.999999 UTC elapsed"}, "Not eligible")
	})

	now = fixtureTime.Add(180 * 24 * time.Hour)
	plan, err := app.PreviewPurge(supervisor, project, []string{old.ID})
	if err != nil || len(plan.Ineligible) != 0 || !slices.Equal(plan.CandidateIDs, []string{old.ID}) ||
		!slices.Equal(plan.Runs, []string{"EXPIRING-RUN"}) || !slices.Equal(plan.ReceiptIDs, []string{receipt.ID}) ||
		!slices.Equal(plan.ReportIDs, []string{frozen.ID}) || len(plan.OrphanBaselines) != 0 ||
		slices.Contains(plan.AuditSequences, young.Sequence) {
		t.Fatalf("wrong expiry projection at exactly 180 elapsed UTC days: %#v %v", plan, err)
	}
	if !plan.ExternalCopiesNotPurged {
		t.Fatal("purge preview did not disclose independently exported report copies")
	}
	removed, err := app.Purge(supervisor, project, []string{old.ID}, plan.Confirmation)
	if err != nil || !reflect.DeepEqual(removed, plan) {
		t.Fatalf("confirmed purge changed its scoped projection: %v", err)
	}
	if _, _, _, err := app.Candidate(supervisor, project, old.ID); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("purged candidate still exists: %v", err)
	}
	if _, err := app.SourceCSV(supervisor, project, receipt.ID); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("purged original upload still exists: %v", err)
	}
	if _, _, err := app.ReportHTML(supervisor, project, frozen.ID); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("purged report still exists: %v", err)
	}
	events, err = app.Audit(supervisor, project)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events, func(event board.AuditEvent) bool {
		return event.Actor == supervisor && event.Action == "retention.purge" && event.Outcome == "success" &&
			event.Time.Equal(now) && slices.Contains(event.References, old.ID) &&
			slices.Contains(event.References, "receipts=1") && slices.Contains(event.References, "runs=1") &&
			slices.Contains(event.References, "reports=1")
	}) {
		t.Fatal("exact-scope purge and counts were not logged")
	}
	recordApprovalInstance(t, "AC-014-04.P001", map[string]any{"candidateAge": "180 days exact"}, "Eligible; scoped records removed; counts logged")

	_, _, saved, err := app.Candidate(supervisor, project, retained.ID)
	if err != nil || saved.Descriptor != descriptor.Descriptor || saved.Descriptor.Digest != old.BaselineDigest {
		t.Fatalf("purge removed or changed the shared approved descriptor: %v", err)
	}
	recordApprovalInstance(t, "AC-014-05.P001", map[string]any{}, "Descriptor retained")
	if !slices.ContainsFunc(events, func(event board.AuditEvent) bool { return reflect.DeepEqual(event, young) }) {
		t.Fatal("old candidate purge discarded a younger audit event")
	}
	recordApprovalInstance(t, "AC-014-06.P001", map[string]any{"auditEventAge": "less than 180 days"}, "Event retained under event-age policy")
	copied, err := os.ReadFile(external)
	if err != nil || !bytes.Equal(copied, html) {
		t.Fatalf("authorized purge touched an exported file beyond its scope: %v", err)
	}
	recordApprovalInstance(t, "AC-014-07.P001", map[string]any{}, "External-copy limitation disclosed")
}

func newCustodySuccessor(t *testing.T, app *board.Board, old board.Candidate, mark string) board.Candidate {
	t.Helper()
	input := board.CandidateInput{Manifest: vectorManifest(), BaselineDigest: old.BaselineDigest, PredecessorID: old.ID}
	input.Manifest.BuildDigest = strings.Repeat(mark, 64)
	next, reused, err := app.CreateCandidate(supervisor, project, input)
	if err != nil || reused || next.ID == old.ID || next.PredecessorID != old.ID {
		t.Fatalf("distinct retained successor fixture failed: reused=%t error=%v", reused, err)
	}
	return next
}

func TestFollowupSharedPurgeRefusesPartialThenAuditsWholeScope(t *testing.T) {
	now := fixtureTime
	app, _ := openBoard(t, func() time.Time { return now })
	first := fixtureCandidate(t, app, vectorBaseline())
	now = now.Add(24 * time.Hour)
	second := newCustodySuccessor(t, app, first, "a")
	stamp := board.FormatUTC(now)
	raw := csvBytes(t,
		resultRow(first, "T1", "1", "RUN-A", "RESULT-A", stamp, stamp, "PASS"),
		resultRow(second, "T1", "1", "RUN-B", "RESULT-B", stamp, stamp, "PASS"))
	receipt := importBytes(t, app, first.ID, raw)
	now = fixtureTime.Add(180 * 24 * time.Hour)
	before, err := app.Candidates(supervisor, project)
	if err != nil || len(before) != 2 {
		t.Fatalf("shared receipt does not have both candidates: %v", err)
	}
	plan, err := app.PreviewPurge(supervisor, project, []string{first.ID})
	if err != nil || !slices.Equal(plan.SharedBlockers, []string{second.ID}) || len(plan.Ineligible) != 0 {
		t.Fatalf("purging A did not identify the younger sharer B: %v", err)
	}
	if _, err := app.Purge(supervisor, project, []string{first.ID}, plan.Confirmation); !errors.Is(err, board.ErrInvalid) {
		t.Fatalf("shared receipt purge incorrectly committed: %v", err)
	}
	after, err := app.Candidates(supervisor, project)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("shared receipt denial changed the candidate catalog: %v", err)
	}
	kept, err := app.Receipt(supervisor, project, receipt.ID)
	if err != nil || !reflect.DeepEqual(kept, receipt) {
		t.Fatalf("shared receipt denial changed its persisted reference: %v", err)
	}
	data, err := app.SourceCSV(supervisor, project, receipt.ID)
	if err != nil || !bytes.Equal(data, raw) {
		t.Fatalf("shared receipt denial changed exact uploaded bytes: %v", err)
	}
	recordApprovalInstance(t, "AC-014-12.P001", map[string]any{}, "Purge blocked with B identified; original bytes and both candidates unchanged")

	third := newCustodySuccessor(t, app, first, "b")
	now = now.Add(24 * time.Hour)
	ids := []string{first.ID, second.ID}
	plan, err = app.PreviewPurge(supervisor, project, ids)
	if err != nil || len(plan.Ineligible) != 0 || len(plan.SharedBlockers) != 0 ||
		len(plan.Runs) != 2 || !slices.Equal(plan.ReceiptIDs, []string{receipt.ID}) {
		t.Fatalf("joint exact expiry did not project shared evidence removal: %#v %v", plan, err)
	}
	if _, err := app.Purge(supervisor, project, ids, plan.Confirmation); err != nil {
		t.Fatal(err)
	}
	found, err := app.Candidates(supervisor, project)
	if err != nil || len(found) != 1 || found[0].ID != third.ID {
		t.Fatalf("joint purge changed candidates outside its scope: %v", err)
	}
	if _, err := app.SourceCSV(supervisor, project, receipt.ID); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("joint purge retained the shared source: %v", err)
	}
	if _, err := app.Receipt(supervisor, project, receipt.ID); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("joint purge retained the shared receipt: %v", err)
	}
	projected, err := app.Assess(supervisor, project, third.ID)
	if err != nil || !projected.PredecessorGone || projected.Candidate.PredecessorID != first.ID || projected.Status != board.Blocked {
		t.Fatalf("joint purge lost the surviving purged-predecessor marker: %v", err)
	}
	events, err := app.Audit(supervisor, project)
	if err != nil || !slices.ContainsFunc(events, func(event board.AuditEvent) bool {
		return event.Actor == supervisor && event.Action == "retention.purge" && event.Outcome == "success" &&
			slices.Contains(event.References, first.ID) && slices.Contains(event.References, second.ID) &&
			slices.Contains(event.References, "runs=2") && slices.Contains(event.References, "receipts=1")
	}) {
		t.Fatalf("joint purge audit did not record both candidates and scoped counts: %v", err)
	}
	recordApprovalInstance(t, "AC-014-13.P001", map[string]any{}, "Shared file and scoped records removed; counts logged; retained predecessor link shows purged marker")
}
