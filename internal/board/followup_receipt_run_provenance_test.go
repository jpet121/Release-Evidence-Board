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

func TestFollowupRetainedReceiptRunOrderAndOriginStayIndependentlyBound(t *testing.T) {
	for _, fault := range []string{"reordered added runs", "reordered existing runs", "duplicate run origin", "existing run source changed"} {
		t.Run(fault, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			_, rows, _ := strings.Cut(string(f.pending), "\n")
			third := reviewRows(f.candidate, "a", "THIRD", Pass, f.at)
			added, _, source := reviewCommit(t, f.app, f.candidate, rows+third)
			if !slices.Equal(added.AddedRuns, []string{"PENDING", "THIRD"}) || len(added.ExistingRuns) != 0 {
				t.Fatalf("fixture did not produce ordered new runs: %+v", added)
			}
			before, err := f.app.Assess(secondActor, secondProject, f.candidate.ID)
			if err != nil || before.LedgerHighWater != 3 || before.Status != Sufficient {
				t.Fatalf("healthy retained run inventory is not sufficient: %v", err)
			}
			chosen, chosenBytes := added, source
			var replay Preview
			if fault != "reordered added runs" {
				alternateRows := strings.Replace(rows+third, ",PASS,,", ",\"PASS\",,", 1)
				chosen, replay, chosenBytes = reviewCommit(t, f.app, f.candidate, alternateRows)
				if chosen.ID == added.ID || len(chosen.AddedRuns) != 0 ||
					!slices.Equal(chosen.ExistingRuns, added.AddedRuns) || !slices.Equal(chosen.PreviousReceipts, []string{added.ID}) {
					t.Fatalf("separate exact-evidence replay did not retain its original receipt link: %+v", chosen)
				}
			}
			var savedRun Run
			changed := chosen
			want := "receipt run ledger ordering is inconsistent"
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				switch fault {
				case "reordered added runs":
					changed.AddedRuns = []string{"THIRD", "PENDING"}
				case "reordered existing runs":
					changed.ExistingRuns = []string{"THIRD", "PENDING"}
				case "duplicate run origin":
					changed.AddedRuns, changed.ExistingRuns = slices.Clone(chosen.ExistingRuns), nil
					changed.PreviousReceipts = nil
					want = "run has more than one originating receipt"
				case "existing run source changed":
					var err error
					savedRun, err = getJSON[Run](tx, runBucket, "PENDING")
					if err != nil {
						return err
					}
					run := savedRun
					run.Rows = slices.Clone(run.Rows)
					run.Rows[0].Fields[colOutcome] = Fail
					want = "retained duplicate rows differ from original CSV"
					return putJSON(tx, runBucket, run.ID, run)
				}
				return putJSON(tx, receiptBucket, chosen.Hash, changed)
			}); err != nil {
				t.Fatal(err)
			}
			if a, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), want) || a.Status != "" {
				t.Errorf("assessment accepted inconsistent retained receipt run provenance %q: %v", want, err)
			}
			if report, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
				t.Errorf("unverified run provenance produced a fresh report: %v", err)
			}
			if raw, err := f.app.SourceCSV(secondActor, secondProject, chosen.ID); err != nil || !bytes.Equal(raw, chosenBytes) {
				t.Errorf("run provenance refusal changed a retained original: %v", err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				if fault == "existing run source changed" {
					return putJSON(tx, runBucket, savedRun.ID, savedRun)
				}
				return putJSON(tx, receiptBucket, chosen.Hash, chosen)
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.app.Revalidate(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			if after, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); err != nil || !reflect.DeepEqual(before, after) {
				t.Errorf("exact repair did not restore the prior assessment: %v", err)
			}
			if replay.Token != "" {
				if committed, err := f.app.CommitImport(secondActor, secondProject, replay.Token, nil); err != nil || !reflect.DeepEqual(committed, chosen) {
					t.Errorf("repair changed the exact committed repeat: %v", err)
				}
			}
			if prior, raw, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || prior != f.report || !bytes.Equal(raw, f.html) {
				t.Errorf("run provenance refusal changed the prior frozen report: %v", err)
			}
		})
	}
}
