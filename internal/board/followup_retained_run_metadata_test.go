package board

import (
	"errors"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupRevalidationRefusesIndependentLedgerAndRunMetadataDamage(t *testing.T) {
	cases := []struct {
		name, diagnostic string
		changeReceipt    func(*Receipt)
		changeRun        func(*Run)
	}{
		{name: "receipt before its run count", diagnostic: "receipt ledger is inconsistent", changeReceipt: func(r *Receipt) { r.LedgerHighWater = 0 }},
		{name: "receipt beyond saved ledger", diagnostic: "receipt ledger is inconsistent", changeReceipt: func(r *Receipt) { r.LedgerHighWater = 2 }},
		{name: "changed sequence", changeRun: func(r *Run) { r.Sequence++ }},
		{name: "changed behavior", changeRun: func(r *Run) { r.BehaviorID = "other-behavior" }},
		{name: "changed finish", changeRun: func(r *Run) { r.Closed = r.Closed.Add(time.Microsecond) }},
		{name: "changed import time", changeRun: func(r *Run) { r.ImportedAt = r.ImportedAt.Add(time.Microsecond) }},
		{name: "different originating receipt", changeRun: func(r *Run) { r.ReceiptID = "I-" + strings.Repeat("a", 64) }},
		{name: "different originating hash", changeRun: func(r *Run) { r.FileHash = strings.Repeat("a", 64) }},
		{name: "run inventory omitted", diagnostic: "stored run inventory differs from CSV evidence"},
	}
	for _, entry := range cases {
		t.Run(entry.name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			var original Run
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				if entry.changeReceipt != nil {
					receipt := f.receipt
					entry.changeReceipt(&receipt)
					return putJSON(tx, receiptBucket, receipt.Hash, receipt)
				}
				var err error
				original, err = getJSON[Run](tx, runBucket, "PRIOR")
				if err != nil {
					return err
				}
				if entry.changeRun == nil {
					return tx.Bucket([]byte(runBucket)).Delete([]byte(original.ID))
				}
				changed := original
				entry.changeRun(&changed)
				return putJSON(tx, runBucket, original.ID, changed)
			}); err != nil {
				t.Fatal(err)
			}
			expected := entry.diagnostic
			if expected == "" {
				expected = "run metadata or complete source row set differs from original CSV"
			}
			if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), expected) {
				t.Errorf("review accepted an independent retained ledger or run change %q: %v", expected, err)
			}
			if assessment, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || assessment.Status != "" {
				t.Errorf("unrepaired run metadata allowed status %q: %v", assessment.Status, err)
			}
			if frozen, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || frozen.ID != "" {
				t.Errorf("unrepaired run metadata allowed report %q: %v", frozen.ID, err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				if entry.changeReceipt != nil {
					return putJSON(tx, receiptBucket, f.receipt.Hash, f.receipt)
				}
				return putJSON(tx, runBucket, original.ID, original)
			}); err != nil {
				t.Fatal(err)
			}
			f.checkReopened(t, true)
		})
	}
}
