package board

import (
	"bytes"
	"errors"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupReviewerCannotClearMalformedDataRevision(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	ids := []string{f.candidate.ID}
	prior, err := f.app.PreviewPurge(secondActor, secondProject, ids)
	if err != nil || prior.Revision == 0 {
		t.Fatalf("fixture lacks the prior data revision: %v", err)
	}
	key := []byte("data_revision")
	var saved []byte
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(metaBucket))
		saved = bytes.Clone(meta.Get(key))
		return meta.Put(key, []byte("invalid-decimal-counter"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.PreviewPurge(secondActor, secondProject, ids); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("fixture did not detect the malformed stored revision through the custody operation: %v", err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
		t.Errorf("reviewer cleared the hold without repairing the malformed data revision: %v", err)
	}
	if assessment, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || assessment.Status != "" {
		t.Errorf("malformed data revision allowed a new assessment: %q, %v", assessment.Status, err)
	}
	if report, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
		t.Errorf("malformed data revision allowed a new frozen report: %q, %v", report.ID, err)
	}
	if report, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(html, f.html) {
		t.Errorf("refusing invalid data revision changed historical report bytes: %v", err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(metaBucket)).Put(key, saved)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("reviewer could not revalidate the repaired data revision: %v", err)
	}
	if plan, err := f.app.PreviewPurge(secondActor, secondProject, ids); err != nil || plan.Revision != prior.Revision {
		t.Errorf("repaired custody report did not recover the original revision: %d, %v", plan.Revision, err)
	}
	if assessment, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); err != nil || assessment.Status != Sufficient {
		t.Errorf("reviewer revalidation did not recover the verified decision: %q, %v", assessment.Status, err)
	}
}
