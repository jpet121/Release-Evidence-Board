package board

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupLiveDecisionRejectsDuplicateSequenceFromIndependentReceipts(t *testing.T) {
	for _, decision := range []string{"assessment", "freeze"} {
		t.Run(decision, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			preview := f.preview(t)
			second, err := f.app.CommitImport(secondActor, secondProject, preview.Token, nil)
			if err != nil || second.ID == f.receipt.ID || second.LedgerHighWater != 2 {
				t.Fatalf("expected an independent second committed original: %v", err)
			}
			before, err := f.app.Assess(secondActor, secondProject, f.candidate.ID)
			if err != nil || before.Status != Sufficient || before.LedgerHighWater != 2 {
				t.Fatalf("healthy sequence fixture was not sufficient: %q, %v", before.Status, err)
			}
			var original Run
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				var err error
				original, err = getJSON[Run](tx, runBucket, "PENDING")
				if err != nil {
					return err
				}
				if original.Sequence != 2 || original.ReceiptID != second.ID || f.receipt.LedgerHighWater != 1 {
					return errors.New("fixture did not retain two original-backed distinct sequences")
				}
				receiptCopy := second
				receiptCopy.LedgerHighWater = f.receipt.LedgerHighWater
				if err := putJSON(tx, receiptBucket, second.Hash, receiptCopy); err != nil {
					return err
				}
				runCopy := original
				runCopy.Sequence = f.receipt.LedgerHighWater
				return putJSON(tx, runBucket, original.ID, runCopy)
			}); err != nil {
				t.Fatal(err)
			}
			if decision == "assessment" {
				if assessed, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || assessed.Status != "" {
					t.Errorf("duplicate originating sequence produced a live assessment %q: %v", assessed.Status, err)
				}
			} else {
				if report, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
					t.Errorf("duplicate originating sequence published a frozen assessment %q: %v", report.Status, err)
				}
			}
			if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
				t.Errorf("independent whole-store check accepted the sequence collision: %v", err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				if err := putJSON(tx, receiptBucket, second.Hash, second); err != nil {
					return err
				}
				return putJSON(tx, runBucket, original.ID, original)
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.app.Revalidate(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			if after, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); err != nil || !reflect.DeepEqual(after, before) {
				t.Errorf("repair did not recover the exact earlier decision: %v", err)
			}
			if csv, err := f.app.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(csv, f.source) {
				t.Errorf("failed check changed the first original: %v", err)
			}
			if csv, err := f.app.SourceCSV(secondActor, secondProject, second.ID); err != nil || !bytes.Equal(csv, f.pending) {
				t.Errorf("failed check changed the second independent original: %v", err)
			}
			if report, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(html, f.html) {
				t.Errorf("checking duplicate sequence changed the previously frozen report: %v", err)
			}
		})
	}
}

func TestFollowupLiveDecisionRejectsDuplicateResultWithinCheckedOriginal(t *testing.T) {
	checkRetainedResultCollision(t, "", true)
}

func TestFollowupDecisionRejectsDistinctResultsForOneRetainedAttempt(t *testing.T) {
	t.Run("fail then pass", func(t *testing.T) { checkRetainedResultCollision(t, "SECOND-ID-SAME-ATTEMPT", true) })
	t.Run("pass then fail", func(t *testing.T) { checkRetainedResultCollision(t, "SECOND-ID-SAME-ATTEMPT", false) })
}

func checkRetainedResultCollision(t *testing.T, secondResultID string, failureFirst bool) {
	t.Helper()
	f := newIntakeCustodyFixture(t)
	preview := f.preview(t)
	second, err := f.app.CommitImport(secondActor, secondProject, preview.Token, nil)
	if err != nil || second.LedgerHighWater != 2 {
		t.Fatalf("fixture lacks a separately committed complete run: %v", err)
	}
	header, records, _ := strings.Cut(string(f.pending), "\n")
	pass, remaining, _ := strings.Cut(records, "\n")
	fail := strings.Replace(pass, ",PASS,,", ",FAIL,,", 1)
	if pass == fail {
		t.Fatal("fixture lacks a passing must-pass row")
	}
	if secondResultID != "" {
		fail = strings.Replace(fail, ",RESPENDING-1,", ","+secondResultID+",", 1)
	}
	first, secondRow := fail, pass
	if !failureFirst {
		first, secondRow = pass, fail
	}
	original := []byte(header + "\n" + first + "\n" + secondRow + "\n" + remaining)
	rows, issues := ParseResultsCSV(original)
	if len(issues) != 0 || len(rows) != 3 || rows[0].TestID() != rows[1].TestID() || rows[0].Attempt() != rows[1].Attempt() ||
		(rows[0].ResultID() == rows[1].ResultID()) != (secondResultID == "") || (rows[0].Outcome() == Fail) != failureFirst {
		t.Fatalf("expected syntactically valid rows with the same planned attempt: %v", issues)
	}
	changed := second
	changed.Hash = digest(original)
	changed.ID = "I-" + changed.Hash
	changed.Rows = len(rows)
	var run Run
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		var err error
		run, err = getJSON[Run](tx, runBucket, "PENDING")
		if err != nil {
			return err
		}
		record := run
		record.Rows, record.ReceiptID, record.FileHash = rows, changed.ID, changed.Hash
		if err := tx.Bucket([]byte(receiptBucket)).Delete([]byte(second.Hash)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(csvBucket)).Delete([]byte(second.Hash)); err != nil {
			return err
		}
		if err := putJSON(tx, receiptBucket, changed.Hash, changed); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(csvBucket)).Put([]byte(changed.Hash), original); err != nil {
			return err
		}
		if secondResultID != "" {
			if err := putJSON(tx, resultBucket, secondResultID, run.ID); err != nil {
				return err
			}
		}
		return putJSON(tx, runBucket, run.ID, record)
	}); err != nil {
		t.Fatal(err)
	}
	if a, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || a.Status != "" {
		t.Errorf("duplicate retained result produced a live assessment %q: %v", a.Status, err)
	}
	if report, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
		t.Errorf("duplicate retained result published a frozen assessment %q: %v", report.Status, err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
		t.Errorf("whole-store validation accepted duplicated result evidence: %v", err)
	}
	if report, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(html, f.html) {
		t.Errorf("checking duplicate results changed the independently frozen report: %v", err)
	}
	if csv, err := f.app.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(csv, f.source) {
		t.Errorf("checking duplicate results changed the independent original: %v", err)
	}
}
