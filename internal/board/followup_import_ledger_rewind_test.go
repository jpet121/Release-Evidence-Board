package board

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupCommitRejectsLiveLedgerRewindAfterValidPreview(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	pending := f.preview(t)
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(metaBucket)).Put([]byte("ledger"), []byte("0"))
	}); err != nil {
		t.Fatal(err)
	}
	if held, err := f.app.AssessmentSuspended(secondActor, secondProject); err != nil || held {
		t.Fatalf("a prior hold would prevent the commit check: held=%t, %v", held, err)
	}
	got, err := f.app.CommitImport(secondActor, secondProject, pending.Token, f.pending)
	if !errors.Is(err, ErrIntegrity) || got.ID != "" {
		t.Errorf("rewound allocator allowed a committed receipt at ledger %d: %v", got.LedgerHighWater, err)
	}
	if a, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || a.Status != "" {
		t.Errorf("rewound allocator allowed assessment %q: %v", a.Status, err)
	}
	if report, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
		t.Errorf("rewound allocator allowed frozen report %q: %v", report.ID, err)
	}
	if _, err := f.app.Receipt(secondActor, secondProject, "I-"+pending.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("refused ledger allocation left a receipt: %v", err)
	}
	if prior, err := f.app.Receipt(secondActor, secondProject, f.receipt.ID); err != nil || !reflect.DeepEqual(prior, f.receipt) {
		t.Errorf("ledger check changed the prior receipt: %v", err)
	}
	if raw, err := f.app.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(raw, f.source) {
		t.Errorf("ledger check changed the prior source bytes: %v", err)
	}
	if report, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(html, f.html) {
		t.Errorf("ledger check changed the frozen report: %v", err)
	}
	if got.ID != "" {
		return
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(metaBucket)).Put([]byte("ledger"), []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("could not recover a fully repaired allocator: %v", err)
	}
	committed, err := f.app.CommitImport(secondActor, secondProject, pending.Token, nil)
	if err != nil || committed.LedgerHighWater != 2 {
		t.Fatalf("rollback lost the unchanged staged import: ledger=%d, %v", committed.LedgerHighWater, err)
	}
	if replay, err := f.app.CommitImport(secondActor, secondProject, pending.Token, nil); err != nil || !reflect.DeepEqual(replay, committed) {
		t.Errorf("exact committed replay changed after allocator repair: %v", err)
	}
}

func TestFollowupPreviewRefusesLedgerBehindAnotherCandidatesReceipt(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	existing := f.preview(t)
	retained, err := f.app.CommitImport(secondActor, secondProject, existing.Token, nil)
	if err != nil || retained.LedgerHighWater != 2 {
		t.Fatalf("fixture did not retain a distinct second allocation: %v", err)
	}
	_, _, approved, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh := reviewCandidate(t, f.app, approved, "b")
	if readback, err := f.app.Assess(secondActor, secondProject, fresh.ID); err != nil ||
		len(readback.Behaviors) != 1 || len(readback.Behaviors[0].Runs) != 0 {
		t.Fatalf("selected candidate is not disjoint from the retained evidence: %v", err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(metaBucket)).Put([]byte("ledger"), []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
	raw := []byte(strings.Join(resultColumns[:], ",") + "\n" + reviewRows(fresh, "b", "DISJOINT", Pass, f.at))
	preview, err := f.app.PreviewImport(secondActor, secondProject, fresh.ID, raw)
	if !errors.Is(err, ErrIntegrity) || preview.Token != "" {
		t.Errorf("disjoint preview ignored a retained ledger witness: token=%t, %v", preview.Token != "", err)
	}
	if preview.Token != "" {
		if receipt, err := f.app.CommitImport(secondActor, secondProject, preview.Token, nil); !errors.Is(err, ErrIntegrity) || receipt.ID != "" {
			t.Errorf("disjoint commit reused the retained ledger position %d: %v", receipt.LedgerHighWater, err)
		}
	}
	if _, err := f.app.Receipt(secondActor, secondProject, "I-"+digest(raw)); !errors.Is(err, ErrNotFound) {
		t.Errorf("refused disjoint import left a committed receipt: %v", err)
	}
	if prior, err := f.app.Receipt(secondActor, secondProject, retained.ID); err != nil || !reflect.DeepEqual(prior, retained) {
		t.Errorf("disjoint allocator check changed the prior receipt: %v", err)
	}
	if source, err := f.app.SourceCSV(secondActor, secondProject, retained.ID); err != nil || !bytes.Equal(source, f.pending) {
		t.Errorf("disjoint allocator check changed the prior original: %v", err)
	}
	if report, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(html, f.html) {
		t.Errorf("disjoint allocator check changed the earlier frozen report: %v", err)
	}
}

func TestFollowupNewImportRefusesMissingReportLedgerSection(t *testing.T) {
	for _, operation := range []string{"preview", "commit"} {
		t.Run(operation, func(t *testing.T) {
			app, baseline, at := reviewBoard(t)
			candidate := reviewCandidate(t, app, baseline, "a")
			prior, priorPreview, original := reviewCommit(t, app, candidate, reviewRows(candidate, "a", "KNOWN", Pass, at))
			raw := []byte(strings.Join(resultColumns[:], ",") + "\n" + reviewRows(candidate, "a", "PROPOSED", Pass, at))
			var pending Preview
			if operation == "commit" {
				var err error
				pending, err = app.PreviewImport(secondActor, secondProject, candidate.ID, raw)
				if err != nil || pending.Token == "" || len(pending.Issues) != 0 {
					t.Fatalf("fixture failed to bind a valid new import before the fault: %v", err)
				}
			}
			if err := app.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(reportBucket)) }); err != nil {
				t.Fatal(err)
			}
			func() {
				defer func() {
					if value := recover(); value != nil {
						t.Errorf("new %s panicked when the report ledger section was missing: %v", operation, value)
					}
				}()
				if operation == "preview" {
					preview, err := app.PreviewImport(secondActor, secondProject, candidate.ID, raw)
					if !errors.Is(err, ErrIntegrity) || preview.Token != "" {
						t.Errorf("missing report ledger section did not refuse the new preview: %v", err)
					}
				} else {
					receipt, err := app.CommitImport(secondActor, secondProject, pending.Token, nil)
					if !errors.Is(err, ErrIntegrity) || receipt.ID != "" {
						t.Errorf("missing report ledger section did not refuse the new commit: %v", err)
					}
				}
			}()
			if held, err := app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
				t.Errorf("missing report ledger section did not set the integrity hold: %v", err)
			}
			if _, err := app.Receipt(secondActor, secondProject, "I-"+digest(raw)); !errors.Is(err, ErrNotFound) {
				t.Errorf("a new import receipt was written despite the missing section: %v", err)
			}
			if replay, err := app.CommitImport(secondActor, secondProject, priorPreview.Token, nil); err != nil || !reflect.DeepEqual(replay, prior) {
				t.Errorf("the independent exact prior receipt could not be recovered: %v", err)
			}
			if source, err := app.SourceCSV(secondActor, secondProject, prior.ID); err != nil || !bytes.Equal(source, original) {
				t.Errorf("the refused import changed the prior source: %v", err)
			}
		})
	}
}
