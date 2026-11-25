package board

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupRevalidationRefusesMissingSectionsAndOrphanOriginal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reason    string
		configure func(*bolt.Tx) error
		repair    func(*bolt.Tx) error
	}{
		{
			name:   "missing_staging_section",
			reason: "storage section missing",
			configure: func(tx *bolt.Tx) error {
				return tx.DeleteBucket([]byte(previewCSV))
			},
			repair: func(tx *bolt.Tx) error {
				_, err := tx.CreateBucket([]byte(previewCSV))
				return err
			},
		},
		{
			name:   "orphan_original",
			reason: "stored evidence companion lacks a primary record",
			configure: func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(csvBucket)).Put([]byte("unowned-test-original"), []byte("orphan original bytes"))
			},
			repair: func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(csvBucket)).Delete([]byte("unowned-test-original"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			if err := f.app.db.Update(tc.configure); err != nil {
				t.Fatal(err)
			}
			if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("reviewer cleared an incomplete storage layout or refused at another phase: %v", err)
			}
			if held, err := f.app.AssessmentSuspended(secondActor, secondProject); !held || err != nil {
				t.Fatalf("incomplete storage layout did not keep the existing hold: held=%t %v", held, err)
			}
			if err := f.app.db.Update(tc.repair); err != nil {
				t.Fatal(err)
			}
			reopened := f.checkReopened(t, true)
			if err := reopened.Revalidate(secondActor, secondProject); err != nil {
				t.Fatalf("reviewer could not revalidate the repaired layout: %v", err)
			}
			if assessment, err := reopened.Assess(secondActor, secondProject, f.candidate.ID); err != nil || assessment.Status != Sufficient {
				t.Fatalf("repaired layout did not recover the prior assessment: %q %v", assessment.Status, err)
			}
		})
	}
}

func TestFollowupRestoreKeepsTargetAbsentWhenItsHeldMetadataWriteRefuses(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	backup, err := f.app.Backup(secondActor, secondProject, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	read := func(path string) []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	original, sidecar := read(backup.Path), read(backup.Path+".sha256")
	retainedDirectories := make([]string, 0, 2)
	for _, key := range []string{"assessment_hold", "last_restore"} {
		dir := t.TempDir()
		if ok := t.Run(key, func(t *testing.T) {
			source := filepath.Join(dir, "owned-source.db")
			if err := os.WriteFile(source, original, 0600); err != nil {
				t.Fatal(err)
			}
			db, err := bolt.Open(source, 0600, &bolt.Options{Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			err = db.Update(func(tx *bolt.Tx) error {
				_, err := tx.Bucket([]byte(metaBucket)).CreateBucket([]byte(key))
				return err
			})
			if err := errors.Join(err, db.Close()); err != nil {
				t.Fatal(err)
			}
			before := read(source)
			verification := []byte(digest(before) + "\n")
			if err := os.WriteFile(source+".sha256", verification, 0600); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(dir, "destination.db")
			var stage string
			var stageHash string
			var sampled bool
			settings := Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time {
				if !sampled {
					sampled = true
					entries, err := os.ReadDir(dir)
					if err != nil {
						t.Errorf("could not observe the already copied restore stage: %v", err)
						return f.at
					}
					for _, entry := range entries {
						if entry.IsDir() && strings.HasPrefix(entry.Name(), ".reb-restore-") {
							stage = filepath.Join(dir, entry.Name(), "store.db")
							stageHash = digest(read(stage))
						}
					}
				}
				return f.at
			}}
			restored, err := RestoreBackup(source, destination, settings)
			if stage == "" || stageHash != digest(before) {
				t.Fatalf("restore did not establish the exact copied stage before its failed database update: stage=%q SHA-256=%s", stage, stageHash)
			}
			if !errors.Is(err, bolt.ErrIncompatibleValue) || restored.IntegrityOK || !restored.RestoredAt.IsZero() {
				t.Fatalf("restore did not refuse the real incompatible metadata write before publication: %+v %v", restored, err)
			}
			t.Logf("observed the copied stage at %s with SHA-256 %s before the real bbolt update refusal: %v", stage, stageHash, err)
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 2 {
				t.Fatalf("failed restore left its target or staged files: %v %v", entries, err)
			}
			if !bytes.Equal(read(source), before) || !bytes.Equal(read(source+".sha256"), verification) {
				t.Fatal("failed restoration changed the supplied backup source or checksum")
			}
		}); !ok {
			return
		}
		retainedDirectories = append(retainedDirectories, dir)
	}
	if !bytes.Equal(read(backup.Path), original) || !bytes.Equal(read(backup.Path+".sha256"), sidecar) {
		t.Fatal("owned restoration attempts changed the prior verified backup")
	}
	f.checkReopened(t, false)
	for _, dir := range retainedDirectories {
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 2 {
			t.Fatalf("restart exposed a publication or abandoned restore stage: %v %v", entries, err)
		}
	}
	if !t.Failed() {
		t.Log(`REB_ACCEPTANCE_INSTANCE {"id":"AC-011-03.P001","case_id":"AC-011-03","parameters":{},"actual":"No visible committed reference; abandoned file cleaned"}`)
	}
}

func TestFollowupRevalidationRequiresTheCompleteRawBackedRunInventory(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	key := []byte(f.receipt.AddedRuns[0])
	var saved []byte
	if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
		t.Fatal(err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		runs := tx.Bucket([]byte(runBucket))
		saved = bytes.Clone(runs.Get(key))
		if len(saved) == 0 {
			return errors.New("fixture lacks the run owned by its committed receipt")
		}
		return runs.Delete(key)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) ||
		!strings.Contains(err.Error(), "stored run inventory differs from CSV evidence") {
		t.Fatalf("revalidation did not find the exact missing raw-backed run: %v", err)
	}
	if held, err := f.app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
		t.Fatalf("incomplete raw-backed run inventory cleared the hold: held=%t %v", held, err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(runBucket)).Put(key, saved)
	}); err != nil {
		t.Fatal(err)
	}
	f.checkReopened(t, true)
}
