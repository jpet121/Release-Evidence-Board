package board

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func setTransactionCounterBoundary(t *testing.T, app *Board, key string) {
	t.Helper()
	if err := app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(metaBucket)).Put([]byte(key), strconv.AppendUint(nil, math.MaxUint64-1, 10))
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFollowupImportLedgerCannotWrapPastFinalRun(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	setTransactionCounterBoundary(t, f.app, "ledger")
	pending, err := f.app.PreviewImport(secondActor, secondProject, f.candidate.ID, f.pending)
	if err != nil || pending.Token == "" || len(pending.Projected) != 1 ||
		pending.Projected[0].LedgerHighWater != math.MaxUint64 {
		t.Fatalf("valid final run did not receive the final ledger projection: %v", err)
	}
	final, err := f.app.CommitImport(secondActor, secondProject, pending.Token, f.pending)
	if err != nil || final.LedgerHighWater != math.MaxUint64 {
		t.Fatalf("could not commit the final ledger position: ledger=%d, %v", final.LedgerHighWater, err)
	}
	overflow := []byte(strings.Join(resultColumns[:], ",") + "\n" + reviewRows(f.candidate, "a", "AFTERMAX", Pass, f.at))
	extra, err := f.app.PreviewImport(secondActor, secondProject, f.candidate.ID, overflow)
	if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrIntegrity) || extra.Token != "" {
		t.Errorf("ledger exhaustion allowed a preview of a new run: token=%t, %v", extra.Token != "", err)
	}
	if extra.Token != "" {
		if receipt, err := f.app.CommitImport(secondActor, secondProject, extra.Token, overflow); !errors.Is(err, ErrInvalid) || receipt.ID != "" {
			t.Errorf("ledger exhaustion published an extra receipt: ledger=%d, %v", receipt.LedgerHighWater, err)
		}
	}
	if receipt, err := f.app.Receipt(secondActor, secondProject, "I-"+digest(overflow)); !errors.Is(err, ErrNotFound) || receipt.ID != "" {
		t.Errorf("an extra receipt was retained after ledger exhaustion: %v", err)
	}
	replay, err := f.app.CommitImport(secondActor, secondProject, pending.Token, nil)
	if err != nil || !reflect.DeepEqual(replay, final) {
		t.Errorf("exhaustion prevented replay of the exact committed final receipt: %v", err)
	}
	assessment, err := f.app.Assess(secondActor, secondProject, f.candidate.ID)
	if err != nil || assessment.Status != Sufficient || assessment.LedgerHighWater != math.MaxUint64 ||
		len(assessment.Behaviors) != 1 || assessment.Behaviors[0].CurrentRunID != "PENDING" {
		t.Errorf("the final ledger decision changed: status=%q ledger=%d, %v", assessment.Status, assessment.LedgerHighWater, err)
	}
	if src, err := f.app.SourceCSV(secondActor, secondProject, final.ID); err != nil || !bytes.Equal(src, f.pending) {
		t.Errorf("ledger exhaustion changed the final retained source: %v", err)
	}
	if report, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(html, f.html) {
		t.Errorf("ledger exhaustion changed the earlier frozen report: %v", err)
	}
}

func TestFollowupApprovalCannotWrapDataRevision(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	_, _, original, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	nextDraft := func(parent ApprovedBaseline) Draft {
		t.Helper()
		draft, err := f.app.CreateDraft(secondActor, secondProject, parent.Baseline, DraftOptions{
			FromBaseline: parent.Descriptor.Digest,
		})
		if err != nil {
			t.Fatal(err)
		}
		return draft
	}
	setTransactionCounterBoundary(t, f.app, "data_revision")
	pending := nextDraft(original)
	final, err := f.app.Approve(secondActor, secondProject, pending.ID, pending.Revision)
	if err != nil || final.DraftID != pending.ID {
		t.Fatalf("could not approve the final data revision: %v", err)
	}
	if plan, err := f.app.PreviewPurge(secondActor, secondProject, nil); err != nil || plan.Revision != math.MaxUint64 {
		t.Fatalf("final data revision is not visible to confirmation previews: %d, %v", plan.Revision, err)
	}
	extra := nextDraft(final)
	if _, err := f.app.Approve(secondActor, secondProject, extra.ID, extra.Revision); !errors.Is(err, ErrInvalid) || errors.Is(err, ErrIntegrity) {
		t.Errorf("approval at exhausted data revision did not refuse: %v", err)
	}
	if plan, err := f.app.PreviewPurge(secondActor, secondProject, nil); err != nil || plan.Revision != math.MaxUint64 {
		t.Errorf("data revision changed after the refused approval: %d, %v", plan.Revision, err)
	}
	currentDraft, err := f.app.Draft(secondActor, secondProject, extra.ID)
	if err != nil || !reflect.DeepEqual(currentDraft, extra) {
		t.Errorf("refused approval marked the pending draft approved: %v", err)
	}
	extraDescriptor, err := extra.Baseline.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Baseline(secondActor, secondProject, extraDescriptor.Digest); !errors.Is(err, ErrNotFound) {
		t.Errorf("refused approval stored an approved descriptor: %v", err)
	}
	if saved, err := f.app.Baseline(secondActor, secondProject, final.Descriptor.Digest); err != nil || !reflect.DeepEqual(saved, final) {
		t.Errorf("refused approval changed earlier retained approval bytes: %v", err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); err != nil {
		t.Errorf("final data revision could not be revalidated: %v", err)
	}
}

func TestFollowupTerminalRevisionRollsBackEvidenceTransactions(t *testing.T) {
	for _, action := range []string{"candidate", "import", "purge"} {
		t.Run(action, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			setting := f.at
			if action == "purge" {
				path := f.app.db.Path()
				if err := f.app.Close(); err != nil {
					t.Fatal(err)
				}
				setting = setting.Add(181 * 24 * time.Hour)
				app, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return setting }})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := app.Close(); err != nil {
						t.Error(err)
					}
				})
				f.app = app
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				value := strconv.AppendUint(nil, math.MaxUint64, 10)
				return tx.Bucket([]byte(metaBucket)).Put([]byte("data_revision"), value)
			}); err != nil {
				t.Fatal(err)
			}
			ids := []string{f.candidate.ID}
			before, err := f.app.PreviewPurge(secondActor, secondProject, ids)
			if err != nil || before.Revision != math.MaxUint64 {
				t.Fatalf("valid final revision was not readable: %v", err)
			}
			switch action {
			case "candidate":
				_, manifest, baseline, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
				if err != nil {
					t.Fatal(err)
				}
				manifest.Manifest.BuildDigest = strings.Repeat("b", 64)
				if _, _, err := f.app.CreateCandidate(secondActor, secondProject, CandidateInput{
					Manifest: manifest.Manifest, BaselineDigest: baseline.Descriptor.Digest,
				}); !errors.Is(err, ErrInvalid) || errors.Is(err, ErrIntegrity) {
					t.Errorf("candidate creation did not refuse final revision capacity: %v", err)
				}
				if stored, err := f.app.Candidates(secondActor, secondProject); err != nil || !reflect.DeepEqual(stored, []Candidate{f.candidate}) {
					t.Errorf("candidate refusal left new catalog data: %v", err)
				}
			case "import":
				pending := f.preview(t)
				if receipt, err := f.app.CommitImport(secondActor, secondProject, pending.Token, nil); !errors.Is(err, ErrInvalid) || receipt.ID != "" {
					t.Errorf("import commit did not refuse final revision capacity: %v", err)
				}
				if _, err := f.app.Receipt(secondActor, secondProject, "I-"+pending.Hash); !errors.Is(err, ErrNotFound) {
					t.Errorf("import refusal left an additional committed receipt: %v", err)
				}
			case "purge":
				if len(before.Ineligible) != 0 || len(before.SharedBlockers) != 0 || len(before.ReportIDs) != 1 {
					t.Fatal("fixture did not make the original evidence eligible for removal")
				}
				if _, err := f.app.Purge(secondActor, secondProject, ids, before.Confirmation); !errors.Is(err, ErrInvalid) {
					t.Errorf("purge did not refuse final revision capacity: %v", err)
				}
			}
			if after, err := f.app.PreviewPurge(secondActor, secondProject, ids); err != nil || !reflect.DeepEqual(after, before) {
				t.Errorf("refused transaction changed the recorded evidence revision or purge plan: %v", err)
			}
			if report, body, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(body, f.html) {
				t.Errorf("refused transaction changed the historical snapshot: %v", err)
			}
			if raw, err := f.app.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(raw, f.source) {
				t.Errorf("refused transaction changed the committed original: %v", err)
			}
			if a, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); err != nil || a.LedgerHighWater != 1 {
				t.Errorf("capacity refusal changed the ledger or blocked valid evidence reads: %v", err)
			}
		})
	}
}
