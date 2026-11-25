package board

import (
	"bytes"
	"errors"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupNewPreviewKeepsEvidenceWhenAbandonedPreviewCleanupFails(t *testing.T) {
	for _, keyspace := range []string{previewBucket, previewCSV} {
		t.Run(keyspace, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			path := f.app.db.Path()
			if err := f.app.Close(); err != nil {
				t.Fatal(err)
			}
			now := f.at
			app, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := app.Close(); err != nil {
					t.Error(err)
				}
			})
			f.app = app
			prior := f.preview(t)
			key := []byte(prior.Token)
			var saved []byte
			if err := app.db.Update(func(tx *bolt.Tx) error {
				bucket := tx.Bucket([]byte(keyspace))
				saved = bytes.Clone(bucket.Get(key))
				if len(saved) == 0 {
					return errors.New("fixture did not retain the original staged input")
				}
				if keyspace == previewBucket {
					return bucket.Put(key, []byte("{"))
				}
				if err := bucket.Delete(key); err != nil {
					return err
				}
				_, err := bucket.CreateBucket(key)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			now = now.Add(30 * time.Minute)
			want := bolt.ErrIncompatibleValue
			if keyspace == previewBucket {
				want = ErrIntegrity
			}
			got, err := app.PreviewImport(secondActor, secondProject, f.candidate.ID, f.pending)
			if !errors.Is(err, want) || got.Token != "" || len(got.Projected) != 0 {
				t.Errorf("failed cleanup still accepted a new import preview: %v", err)
			}
			if _, err := app.Receipt(secondActor, secondProject, "I-"+prior.Hash); !errors.Is(err, ErrNotFound) {
				t.Errorf("failed cleanup produced a committed receipt: %v", err)
			}
			if report, body, err := app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(body, f.html) {
				t.Errorf("failed cleanup changed the frozen report: %v", err)
			}
			if raw, err := app.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(raw, f.source) {
				t.Errorf("failed cleanup changed the committed original: %v", err)
			}
			if err := app.db.Update(func(tx *bolt.Tx) error {
				bucket := tx.Bucket([]byte(keyspace))
				if keyspace == previewCSV {
					if err := bucket.DeleteBucket(key); err != nil {
						return err
					}
				}
				return bucket.Put(key, saved)
			}); err != nil {
				t.Fatal(err)
			}
			if err := app.Revalidate(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			next, err := app.PreviewImport(secondActor, secondProject, f.candidate.ID, f.pending)
			if err != nil || next.Token == "" || next.Token == prior.Token {
				t.Fatalf("repair did not permit an independent fresh confirmation: %v", err)
			}
			if _, err := app.CommitImport(secondActor, secondProject, prior.Token, nil); !errors.Is(err, ErrNotFound) {
				t.Errorf("cleanup retained the expired binding: %v", err)
			}
			if receipt, err := app.CommitImport(secondActor, secondProject, next.Token, nil); err != nil || receipt.LedgerHighWater != 2 {
				t.Errorf("fresh preview did not recover exact staging and commit: %v", err)
			}
		})
	}
}
