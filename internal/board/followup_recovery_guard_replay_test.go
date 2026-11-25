package board

import (
	"bytes"
	"errors"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupCommitRefusesPlanDamageAfterSuccessfulPreview(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	pending := f.preview(t)
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return putJSON(tx, materialBucket, f.candidate.MaterialDigest, "C-absent")
	}); err != nil {
		t.Fatal(err)
	}
	if receipt, err := f.app.CommitImport(secondActor, secondProject, pending.Token, f.pending); !errors.Is(err, ErrIntegrity) || receipt.ID != "" {
		t.Errorf("commit after a failed transaction plan returned an apparent receipt %q: %v", receipt.ID, err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return putJSON(tx, materialBucket, f.candidate.MaterialDigest, f.candidate.ID)
	}); err != nil {
		t.Fatal(err)
	}
	f.checkReopened(t, true)
}

func TestFollowupRevalidationCannotClearOrphanFrozenBinding(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
		t.Fatal(err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return putJSON(tx, reportMetaBucket, "RPT-2", frozenReportBinding{
			MetadataSHA256: digest([]byte("missing report metadata")), HTMLSHA256: f.report.SHA256,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
		t.Errorf("reviewer revalidated a store containing an orphan frozen binding: %v", err)
	}
	if held, err := f.app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
		t.Errorf("orphan frozen binding did not keep assessment held: held=%t, %v", held, err)
	}
	report, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID)
	if err != nil || report != f.report || !bytes.Equal(html, f.html) {
		t.Errorf("refusing revalidation changed the valid prior frozen artifact: %v", err)
	}
}

func TestFollowupRevalidationKeepsHoldWhenStoredMarkerCannotDelete(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	holdKey := []byte("assessment_hold")
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.Bucket([]byte(metaBucket)).CreateBucket(holdKey); err != nil {
			return err
		}
		return tx.Bucket([]byte(csvBucket)).Put([]byte(f.receipt.Hash), []byte("temporary custody fault"))
	}); err != nil {
		t.Fatal(err)
	}
	if raw, err := f.app.SourceCSV(secondActor, secondProject, f.receipt.ID); len(raw) != 0 ||
		!errors.Is(err, ErrIntegrity) || !errors.Is(err, bolt.ErrIncompatibleValue) {
		t.Fatalf("fixture did not produce the real process-local hold: %d bytes, %v", len(raw), err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(csvBucket)).Put([]byte(f.receipt.Hash), f.source)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
		t.Errorf("revalidation did not reject the incompatible saved hold type: %v", err)
	}
	if held, err := f.app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
		t.Errorf("failed hold deletion cleared the live integrity hold: held=%t, %v", held, err)
	}
	report, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID)
	if err != nil || report != f.report || !bytes.Equal(html, f.html) {
		t.Errorf("failed hold deletion changed the prior frozen artifact: %v", err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(metaBucket)).DeleteBucket(holdKey)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("reviewer could not revalidate after actual hold storage repair: %v", err)
	}
	if a, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); err != nil || a.Status != Sufficient {
		t.Errorf("reviewer revalidation did not recover the actual checked evidence: %q, %v", a.Status, err)
	}
}
