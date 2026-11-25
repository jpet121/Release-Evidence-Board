package board

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupRevalidateRefusesNestedCounterSlots(t *testing.T) {
	for _, field := range []string{"data_revision", "import_rejections", "report_failures"} {
		t.Run(field, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			key := []byte(field)
			var saved []byte
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				meta := tx.Bucket([]byte(metaBucket))
				saved = bytes.Clone(meta.Get(key))
				if err := meta.Delete(key); err != nil {
					return err
				}
				_, err := meta.CreateBucket(key)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			assertMetadataFaultKeepsHold(t, f, field)

			dir := t.TempDir()
			source, destination := filepath.Join(dir, "source.db"), filepath.Join(dir, "restored.db")
			if err := f.app.db.View(func(tx *bolt.Tx) error { return tx.CopyFile(source, 0600) }); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(source+".sha256", []byte(fmt.Sprintf("%x\n", sha256.Sum256(data))), 0600); err != nil {
				t.Fatal(err)
			}
			settings := Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return f.at }}
			result, err := RestoreBackup(source, destination, settings)
			if err != nil {
				t.Fatal(err)
			}
			if result.IntegrityOK {
				t.Errorf("checksummed restore accepted nested %s", field)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				meta := tx.Bucket([]byte(metaBucket))
				if err := meta.DeleteBucket(key); err != nil {
					return err
				}
				if saved == nil {
					return nil
				}
				return meta.Put(key, saved)
			}); err != nil {
				t.Fatal(err)
			}
			assertMetadataRepairRecoversAssessment(t, f)
		})
	}
}

func TestFollowupRevalidateRefusesKnownMalformedRecoveryTimestamps(t *testing.T) {
	for _, field := range []string{"last_backup", "last_restore", "last_drill"} {
		t.Run(field, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			key := []byte(field)
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(metaBucket)).Put(key, []byte("invalid-utc-timestamp"))
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.app.Health(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("health did not observe the malformed %s timestamp: %v", field, err)
			}
			assertMetadataFaultKeepsHold(t, f, field)
			if err := f.app.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(metaBucket)).Delete(key) }); err != nil {
				t.Fatal(err)
			}
			assertMetadataRepairRecoversAssessment(t, f)
		})
	}
}

func TestFollowupRevalidateAcceptsAbsentOptionalMetadata(t *testing.T) {
	app, err := Open(filepath.Join(t.TempDir(), "new.db"), Settings{ProjectID: secondProject, ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := app.Bootstrap(secondActor, "synthetic-review-password"); err != nil {
		t.Fatal(err)
	}
	if err := app.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("legitimately absent optional fields blocked reviewer revalidation: %v", err)
	}
	h, err := app.Health(secondActor, secondProject)
	if err != nil || !h.LastBackup.IsZero() || !h.LastRestore.IsZero() || !h.LastDrill.IsZero() || h.Hold ||
		h.ImportRejections != 0 || h.ReportFailures != 0 {
		t.Errorf("empty store did not preserve optional metadata defaults: %v", err)
	}
}

func assertMetadataFaultKeepsHold(t *testing.T, f intakeCustodyFixture, field string) {
	t.Helper()
	if held, err := f.app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
		t.Fatalf("fixture did not enter initial hold: %v", err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
		t.Errorf("reviewer cleared hold with invalid %s: %v", field, err)
	}
	if a, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || a.Status != "" {
		t.Errorf("invalid %s allowed assessment %q: %v", field, a.Status, err)
	}
	if report, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
		t.Errorf("invalid %s allowed frozen report %q: %v", field, report.ID, err)
	}
	if report, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil ||
		report != f.report || !bytes.Equal(html, f.html) {
		t.Errorf("metadata fault changed historical report bytes: %v", err)
	}
}

func assertMetadataRepairRecoversAssessment(t *testing.T, f intakeCustodyFixture) {
	t.Helper()
	if err := f.app.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("reviewer did not clear the repaired metadata fault: %v", err)
	}
	if a, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); err != nil || a.Status != Sufficient {
		t.Errorf("reviewer repair did not recover the verified assessment: %q, %v", a.Status, err)
	}
}
