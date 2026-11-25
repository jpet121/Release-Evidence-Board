package board

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupPurgeRollsBackAtIndependentStorageFailures(t *testing.T) {
	for _, target := range []string{purgedRunBucket, tombstoneBucket, previewCSV} {
		t.Run(target, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			file := f.app.db.Path()
			if err := f.app.Close(); err != nil {
				t.Fatal(err)
			}
			aged := f.at.Add(181 * 24 * time.Hour)
			settings := Settings{
				ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return aged },
			}
			app, err := Open(file, settings)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := app.Close(); err != nil {
					t.Error(err)
				}
			})
			pending, err := app.PreviewImport(secondActor, secondProject, f.candidate.ID, f.pending)
			if err != nil || pending.Token == "" || len(pending.Issues) != 0 {
				t.Fatalf("fixture did not stage the fresh confirmation: %v", err)
			}
			before, err := app.Assess(secondActor, secondProject, f.candidate.ID)
			if err != nil || before.LedgerHighWater != 1 {
				t.Fatalf("fixture lacks the prior ledger: %v", err)
			}
			ids := []string{f.candidate.ID}
			plan, err := app.PreviewPurge(secondActor, secondProject, ids)
			if err != nil || len(plan.Runs) != 1 || len(plan.AuditSequences) == 0 || len(plan.Ineligible) != 0 ||
				len(plan.SharedBlockers) != 0 || !slices.Equal(plan.ReportIDs, []string{f.report.ID}) {
				t.Fatalf("fixture did not produce an eligible real purge: %v", err)
			}
			keys := map[string]string{
				purgedRunBucket: plan.Runs[0], tombstoneBucket: f.candidate.ID, previewCSV: pending.Token,
			}
			key := []byte(keys[target])
			expectExisting := target == previewCSV
			var original []byte
			if err := app.db.Update(func(tx *bolt.Tx) error {
				records := tx.Bucket([]byte(target))
				original = bytes.Clone(records.Get(key))
				if (original != nil) != expectExisting {
					return fmt.Errorf("unexpected fixture occupancy in %s", target)
				}
				if err := records.Delete(key); err != nil {
					return err
				}
				_, err := records.CreateBucket(key)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if actual, err := app.PreviewPurge(secondActor, secondProject, ids); err != nil || !reflect.DeepEqual(actual, plan) {
				t.Fatalf("fault changed the preview rather than the transaction write: %v", err)
			}
			if _, err := app.Purge(secondActor, secondProject, ids, plan.Confirmation); !errors.Is(err, bolt.ErrIncompatibleValue) {
				t.Errorf("purge did not refuse the incompatible %s write: %v", target, err)
			}
			if err := app.db.Update(func(tx *bolt.Tx) error {
				records := tx.Bucket([]byte(target))
				if err := records.DeleteBucket(key); err != nil {
					return err
				}
				if original == nil {
					return nil
				}
				return records.Put(key, original)
			}); err != nil {
				t.Fatal(err)
			}
			if current, err := app.PreviewPurge(secondActor, secondProject, ids); err != nil || !reflect.DeepEqual(current, plan) {
				t.Errorf("failed purge changed the retained plan after repairing the isolated fault: %v", err)
			}
			if err := app.Close(); err != nil {
				t.Fatal(err)
			}
			restored, err := Open(file, settings)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := restored.Close(); err != nil {
					t.Error(err)
				}
			})
			if after, err := restored.Assess(secondActor, secondProject, f.candidate.ID); err != nil || !reflect.DeepEqual(after, before) {
				t.Errorf("failed purge changed the prior assessment or ledger: %v", err)
			}
			if report, raw, err := restored.ReportHTML(secondActor, secondProject, f.report.ID); err != nil ||
				report != f.report || !bytes.Equal(raw, f.html) {
				t.Errorf("failed purge changed frozen report bytes: %v", err)
			}
			if source, err := restored.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(source, f.source) {
				t.Errorf("failed purge changed the prior original: %v", err)
			}
			receipt, err := restored.CommitImport(secondActor, secondProject, pending.Token, nil)
			if err != nil || receipt.LedgerHighWater != 2 {
				t.Fatalf("failed purge lost the pending original or its confirmation: %v", err)
			}
			if source, err := restored.SourceCSV(secondActor, secondProject, receipt.ID); err != nil || !bytes.Equal(source, f.pending) {
				t.Errorf("recovered pending confirmation stored different source bytes: %v", err)
			}
		})
	}
}
