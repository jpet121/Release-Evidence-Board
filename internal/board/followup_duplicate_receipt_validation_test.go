package board

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupDuplicateReceiptLinksAreCheckedBeforeClearingHold(t *testing.T) {
	cases := []struct {
		name, check string
		edit        func(*Receipt)
	}{
		{"schema", "receipt row count or parser version differs", func(r *Receipt) { r.Schema = "unknown-schema" }},
		{"workspace", "receipt workspace or historical candidate references are inconsistent", func(r *Receipt) { r.OpenCandidateID = "C-missing" }},
		{"historical candidate", "receipt workspace or historical candidate references are inconsistent", func(r *Receipt) { r.HistoricalCandidates = slices.Clone(r.CandidateIDs) }},
		{"originating receipt", "receipt references differ from originating run receipts", func(r *Receipt) { r.PreviousReceipts = nil }},
	}
	for _, entry := range cases {
		t.Run(entry.name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			_, additional, found := strings.Cut(string(f.pending), "\n")
			if !found || len(additional) == 0 {
				t.Fatal("fixture lacks an additional complete run")
			}
			mixed := append(bytes.Clone(f.source), additional...)
			preview, err := f.app.PreviewImport(secondActor, secondProject, f.candidate.ID, mixed)
			if err != nil || preview.Token == "" || len(preview.Issues) != 0 {
				t.Fatalf("could not preview a duplicate and a new run together: %v", err)
			}
			receipt, err := f.app.CommitImport(secondActor, secondProject, preview.Token, mixed)
			if err != nil || !slices.Equal(receipt.ExistingRuns, []string{"PRIOR"}) ||
				!slices.Equal(receipt.AddedRuns, []string{"PENDING"}) ||
				!slices.Equal(receipt.PreviousReceipts, []string{f.receipt.ID}) {
				t.Fatalf("receipt does not prove the intended original and added runs: %v", err)
			}
			if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			damaged := receipt
			entry.edit(&damaged)
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				return putJSON(tx, receiptBucket, receipt.Hash, damaged)
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), entry.check) {
				t.Errorf("reviewer did not refuse %q before clearing the hold: %v", entry.check, err)
			}
			if a, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || a.Status != "" {
				t.Errorf("damaged duplicate history allowed a new assessment %q: %v", a.Status, err)
			}
			if raw, err := f.app.SourceCSV(secondActor, secondProject, receipt.ID); err != nil || !bytes.Equal(raw, mixed) {
				t.Errorf("review of the damaged receipt changed its original bytes: %v", err)
			}
			if report, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil ||
				report != f.report || !bytes.Equal(html, f.html) {
				t.Errorf("review of the damaged receipt changed the historical report: %v", err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				return putJSON(tx, receiptBucket, receipt.Hash, receipt)
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.app.Revalidate(secondActor, secondProject); err != nil {
				t.Fatalf("reviewer could not clear the hold after exact repair: %v", err)
			}
			replay, err := f.app.CommitImport(secondActor, secondProject, preview.Token, nil)
			if err != nil || !reflect.DeepEqual(replay, receipt) {
				t.Errorf("repaired duplicate intake did not recover the exact receipt: %v", err)
			}
		})
	}
}
