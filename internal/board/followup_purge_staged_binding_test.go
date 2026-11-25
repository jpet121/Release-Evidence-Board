package board

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupPurgeRollsBackWhenStagedCleanupCannotReadBinding(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	path := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	now := f.at.Add(181 * 24 * time.Hour)
	app, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return now }})
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
		t.Fatalf("eligible candidate could not stage independent evidence: %v", err)
	}
	scope := []string{f.candidate.ID}
	plan, err := app.PreviewPurge(secondActor, secondProject, scope)
	if err != nil || len(plan.Ineligible) != 0 || len(plan.SharedBlockers) != 0 ||
		len(plan.Runs) != 1 || len(plan.ReceiptIDs) != 1 || len(plan.ReportIDs) != 1 {
		t.Fatalf("fixture did not create an eligible exact purge plan: %v", err)
	}
	before, err := app.Assess(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte(pending.Token)
	var binding []byte
	if err := app.db.Update(func(tx *bolt.Tx) error {
		stored := tx.Bucket([]byte(previewBucket))
		binding = bytes.Clone(stored.Get(key))
		if binding == nil {
			return errors.New("pending binding absent")
		}
		return stored.Put(key, []byte("{"))
	}); err != nil {
		t.Fatal(err)
	}
	if current, err := app.PreviewPurge(secondActor, secondProject, scope); err != nil || !reflect.DeepEqual(current, plan) {
		t.Fatalf("staged binding fault changed a pre-transaction prerequisite: %v", err)
	}
	if _, err := app.Purge(secondActor, secondProject, scope, plan.Confirmation); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("purge accepted unreadable pending cleanup after confirmation: %v", err)
	}
	if err := app.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(previewBucket)).Put(key, binding) }); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	if held, err := reopened.AssessmentSuspended(secondActor, secondProject); !held || err != nil {
		t.Fatalf("failed pending cleanup did not retain review hold: %v", err)
	}
	if err := reopened.Revalidate(secondActor, secondProject); err != nil {
		t.Fatal(err)
	}
	if after, err := reopened.Assess(secondActor, secondProject, f.candidate.ID); err != nil || !reflect.DeepEqual(after, before) {
		t.Errorf("failed cleanup changed the earlier assessment or ledger: %v", err)
	}
	if raw, err := reopened.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(raw, f.source) {
		t.Errorf("failed cleanup changed the prior original: %v", err)
	}
	if report, html, err := reopened.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(html, f.html) {
		t.Errorf("failed cleanup changed the prior frozen decision: %v", err)
	}
	receipt, err := reopened.CommitImport(secondActor, secondProject, pending.Token, nil)
	if err != nil || receipt.LedgerHighWater != 2 {
		t.Fatalf("rollback lost pending intake when its binding was repaired: %v", err)
	}
	if raw, err := reopened.SourceCSV(secondActor, secondProject, receipt.ID); err != nil || !bytes.Equal(raw, f.pending) {
		t.Errorf("repaired pending intake no longer had its exact original: %v", err)
	}
}
