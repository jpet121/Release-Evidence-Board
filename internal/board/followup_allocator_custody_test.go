package board

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupRevalidateRefusesRewoundDraftAllocator(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	_, _, baseline, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil || baseline.DraftID != "D-1" {
		t.Fatalf("fixture did not produce the first approved draft: %v", err)
	}
	original, err := f.app.Draft(secondActor, secondProject, baseline.DraftID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
		t.Fatal(err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(draftBucket)).SetSequence(0)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
		t.Errorf("reviewer cleared a hold with an unrepaired draft allocator: %v", err)
	}
	if assessment, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || assessment.Status != "" {
		t.Errorf("rewound draft allocator allowed a new assessment: status=%q, %v", assessment.Status, err)
	}
	draft, err := f.app.Draft(secondActor, secondProject, baseline.DraftID)
	if err != nil || !reflect.DeepEqual(draft, original) {
		t.Errorf("checking the draft allocator changed approved draft history: %v", err)
	}
	report, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID)
	if err != nil || report != f.report || !bytes.Equal(html, f.html) {
		t.Errorf("checking the draft allocator changed verified report bytes: %v", err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(draftBucket)).SetSequence(1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("reviewer could not revalidate the repaired draft sequence: %v", err)
	}
	if assessment, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); err != nil || assessment.Status != Sufficient {
		t.Errorf("repair did not restore sufficient assessment: %q, %v", assessment.Status, err)
	}
}

func TestFollowupDraftAllocatorRefusesAfterFinalID(t *testing.T) {
	app, baseline, _ := reviewBoard(t)
	if err := app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(draftBucket)).SetSequence(math.MaxUint64 - 1)
	}); err != nil {
		t.Fatal(err)
	}
	final, err := app.CreateDraft(secondActor, secondProject, baseline.Baseline, DraftOptions{})
	if err != nil || final.ID != "D-18446744073709551615" {
		t.Fatalf("could not allocate the final draft identifier: %q, %v", final.ID, err)
	}
	if err := app.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("valid final draft prevents integrity verification: %v", err)
	}
	attempt, err := app.CreateDraft(secondActor, secondProject, baseline.Baseline, DraftOptions{})
	if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrIntegrity) || attempt.ID != "" {
		t.Errorf("exhausted draft allocator published a wrapped draft: %q, %v", attempt.ID, err)
	}
	if _, err := app.Draft(secondActor, secondProject, "D-0"); !errors.Is(err, ErrNotFound) {
		t.Errorf("wrapped draft became a retained record: %v", err)
	}
	saved, err := app.Draft(secondActor, secondProject, final.ID)
	if err != nil || !reflect.DeepEqual(saved, final) {
		t.Errorf("capacity refusal changed the final draft: %v", err)
	}
}

func TestFollowupAuditRejectsRewindIntoPurgedGap(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	_, _, baseline, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	file := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	now := f.at.Add(181 * 24 * time.Hour)
	app, err := Open(file, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	retained := reviewCandidate(t, app, baseline, "b")
	reviewCommit(t, app, retained, reviewRows(retained, "b", "RETAINED", Pass, now))
	report, err := app.Freeze(secondActor, secondProject, retained.ID)
	if err != nil || report.Status != Sufficient {
		t.Fatalf("fixture could not retain a fresh sufficient report: %v", err)
	}
	report, html, err := app.ReportHTML(secondActor, secondProject, report.ID)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{f.candidate.ID}
	preview, err := app.PreviewPurge(secondActor, secondProject, ids)
	if err != nil || len(preview.AuditSequences) == 0 {
		t.Fatalf("fixture could not preview removal of the expired audit range: %v", err)
	}
	if _, err := app.Purge(secondActor, secondProject, ids, preview.Confirmation); err != nil {
		t.Fatal(err)
	}
	if err := app.SuspendAssessments(secondActor, secondProject); err != nil {
		t.Fatal(err)
	}
	audit, err := app.Audit(secondActor, secondProject)
	if err != nil || len(audit) == 0 || audit[0].Sequence <= 1 {
		t.Fatalf("public retention did not leave a gap before surviving audit records: %v", err)
	}
	if err := app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(auditBucket)).SetSequence(0)
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
		t.Errorf("reviewer cleared a hold with a rewound audit allocator: %v", err)
	}
	if assessment, err := app.Assess(secondActor, secondProject, retained.ID); !errors.Is(err, ErrIntegrity) || assessment.Status != "" {
		t.Errorf("rewound audit allocator allowed a new assessment: status=%q, %v", assessment.Status, err)
	}
	readback, err := app.Audit(secondActor, secondProject)
	if err != nil || !reflect.DeepEqual(readback, audit) {
		t.Errorf("rewound audit allocator appended a record inside the expired gap: %v", err)
	}
	after, raw, err := app.ReportHTML(secondActor, secondProject, report.ID)
	if err != nil || after != report || !bytes.Equal(raw, html) {
		t.Errorf("checking the audit allocator changed verified historical evidence: %v", err)
	}
	if err := app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(auditBucket)).SetSequence(audit[len(audit)-1].Sequence)
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("reviewer could not revalidate the genuine repaired audit gap: %v", err)
	}
	repaired, err := app.Audit(secondActor, secondProject)
	if err != nil || len(repaired) != len(audit)+1 || repaired[len(audit)].Sequence != audit[len(audit)-1].Sequence+1 {
		t.Errorf("repaired allocator failed to append after all retained audit records: %v", err)
	}

	// Once the valid reviewer action released the prior hold, a fresh
	// corruption must be detected during opening, before any new audit.
	if err := app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(auditBucket)).SetSequence(0)
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(file, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	if held, err := reopened.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
		t.Errorf("opening did not detect the rewound audit allocator: held=%t, %v", held, err)
	}
	readback, err = reopened.Audit(secondActor, secondProject)
	if err != nil || !reflect.DeepEqual(readback, repaired) {
		t.Errorf("opening appended into the expired audit gap: %v", err)
	}
}

func TestFollowupAuditAllocatorRefusesAfterFinalSequence(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(auditBucket)).SetSequence(math.MaxUint64 - 1)
	}); err != nil {
		t.Fatal(err)
	}
	const password = "synthetic-audit-password"
	if err := f.app.SetAccount(secondActor, "terminal-audit-viewer", password, []Role{Viewer}); err != nil {
		t.Fatalf("failed to record the last audit event: %v", err)
	}
	final, err := f.app.Audit(secondActor, secondProject)
	if err != nil || len(final) == 0 || final[len(final)-1].Sequence != math.MaxUint64 {
		t.Fatalf("fixture did not record the final audit sequence: %v", err)
	}
	if err := f.app.SetAccount(secondActor, "cannot-record-viewer", password, []Role{Viewer}); !errors.Is(err, ErrInvalid) || errors.Is(err, ErrIntegrity) {
		t.Errorf("exhausted audit allocator allowed a wrapping mutation: %v", err)
	}
	if _, err := f.app.Account("cannot-record-viewer"); !errors.Is(err, ErrNotFound) {
		t.Errorf("mutation committed without an appendable audit sequence: %v", err)
	}
	after, err := f.app.Audit(secondActor, secondProject)
	if err != nil || !reflect.DeepEqual(after, final) {
		t.Errorf("capacity refusal changed verified terminal audit history: %v", err)
	}
}
