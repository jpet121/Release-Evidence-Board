package board

import (
	"bytes"
	"errors"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupReviewerCannotClearMalformedHealthCounters(t *testing.T) {
	for _, name := range []string{"import_rejections", "report_failures"} {
		t.Run(name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			if _, err := f.app.Health(secondActor, secondProject); err != nil {
				t.Fatalf("baseline health does not read the verified store: %v", err)
			}
			key := []byte(name)
			var original []byte
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				meta := tx.Bucket([]byte(metaBucket))
				original = bytes.Clone(meta.Get(key))
				return meta.Put(key, []byte("invalid-duration-independent-counter"))
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.app.Health(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("public health did not detect the malformed %s counter: %v", name, err)
			}
			if held, err := f.app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
				t.Fatalf("observed health fault did not set the hold: held=%t, %v", held, err)
			}
			if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
				t.Errorf("reviewer cleared the hold without repairing %s: %v", name, err)
			}
			if a, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || a.Status != "" {
				t.Errorf("malformed %s allowed a new assessment: %q, %v", name, a.Status, err)
			}
			if report, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
				t.Errorf("malformed %s allowed a new frozen report: %q, %v", name, report.ID, err)
			}
			if report, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil ||
				report != f.report || !bytes.Equal(html, f.html) {
				t.Errorf("health counter fault changed verified historical report bytes: %v", err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				meta := tx.Bucket([]byte(metaBucket))
				if original == nil {
					return meta.Delete(key)
				}
				return meta.Put(key, original)
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.app.Revalidate(secondActor, secondProject); err != nil {
				t.Fatalf("reviewer could not revalidate after exact repair: %v", err)
			}
			if health, err := f.app.Health(secondActor, secondProject); err != nil || health.Hold {
				t.Errorf("repaired store did not recover ordinary health: held=%t, %v", health.Hold, err)
			}
			if a, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); err != nil || a.Status != Sufficient {
				t.Errorf("repaired store did not recover the prior decision: %q, %v", a.Status, err)
			}
		})
	}
}
