package board

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFrozenMetadataRejectsMissingOrAlteredHTMLPayload(t *testing.T) {
	for _, damage := range []string{"absent", "altered"} {
		t.Run(damage, func(t *testing.T) {
			b, baseline, at := reviewBoard(t)
			type frozenEvidence struct {
				candidate Candidate
				receipt   Receipt
				original  []byte
				report    Report
				html      []byte
			}
			create := func(mark, run string) frozenEvidence {
				t.Helper()
				candidate := reviewCandidate(t, b, baseline, mark)
				receipt, _, original := reviewCommit(t, b, candidate, reviewRows(candidate, mark, run, "PASS", at))
				decision, err := b.Assess(secondActor, secondProject, candidate.ID)
				if err != nil || decision.Status != Sufficient {
					t.Fatalf("initial assessment for %s: status=%s error=%v", mark, decision.Status, err)
				}
				report, err := b.Freeze(secondActor, secondProject, candidate.ID)
				if err != nil || report.Status != Sufficient || report.CandidateID != candidate.ID {
					t.Fatalf("initial frozen report for %s: status=%s error=%v", mark, report.Status, err)
				}
				view, html, err := b.ReportHTML(secondActor, secondProject, report.ID)
				if err != nil || len(html) == 0 || view.SHA256 != report.SHA256 || !bytes.Contains(html, []byte(Sufficient)) {
					t.Fatalf("initial historical read for %s: bytes=%d error=%v", mark, len(html), err)
				}
				return frozenEvidence{candidate: candidate, receipt: receipt, original: original, report: view, html: html}
			}

			damaged := create("a", "PAYLOAD-TARGET")
			healthy := create("b", "PAYLOAD-CONTROL")
			protectedBefore := snapshotOtherFrozenEvidence(t, b, damaged.report.ID)
			key := []byte(damaged.report.ID)
			if err := b.db.Update(func(tx *bolt.Tx) error {
				payloads := tx.Bucket([]byte(htmlBucket))
				if damage == "absent" {
					return payloads.Delete(key)
				}
				changed := append(bytes.Clone(damaged.html), []byte("<!-- synthetic payload damage -->")...)
				return payloads.Put(key, changed)
			}); err != nil {
				t.Fatal(err)
			}
			if got := snapshotOtherFrozenEvidence(t, b, damaged.report.ID); !reflect.DeepEqual(got, protectedBefore) {
				t.Fatal("fixture fault changed evidence outside the selected HTML payload")
			}
			if held, err := b.AssessmentSuspended(secondActor, secondProject); err != nil || held {
				t.Fatalf("assessment unexpectedly held before the damaged report read: held=%t error=%v", held, err)
			}

			view, raw, err := b.ReportHTML(secondActor, secondProject, damaged.report.ID)
			if !errors.Is(err, ErrIntegrity) || len(raw) != 0 || view.ID != "" {
				t.Fatalf("unverified frozen HTML was not refused: damage=%s report=%q bytes=%d error=%v", damage, view.ID, len(raw), err)
			}
			if held, err := b.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
				t.Fatalf("damaged report read did not latch the assessment hold: held=%t error=%v", held, err)
			}
			for _, fixture := range []frozenEvidence{damaged, healthy} {
				if decision, err := b.Assess(secondActor, secondProject, fixture.candidate.ID); !errors.Is(err, ErrIntegrity) || decision.Status == Sufficient {
					t.Fatalf("new assessment escaped the hold: candidate=%s status=%s error=%v", fixture.candidate.ID, decision.Status, err)
				}
				if report, err := b.Freeze(secondActor, secondProject, fixture.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
					t.Fatalf("new frozen export escaped the hold: candidate=%s report=%q error=%v", fixture.candidate.ID, report.ID, err)
				}
				receipt, err := b.Receipt(secondActor, secondProject, fixture.receipt.ID)
				if err != nil || !reflect.DeepEqual(receipt, fixture.receipt) {
					t.Fatalf("hold disturbed a retained receipt: id=%s error=%v", fixture.receipt.ID, err)
				}
				original, err := b.SourceCSV(secondActor, secondProject, receipt.ID)
				if err != nil || !bytes.Equal(original, fixture.original) {
					t.Fatalf("hold disturbed retained source bytes: id=%s error=%v", receipt.ID, err)
				}
			}
			view, raw, err = b.ReportHTML(secondActor, secondProject, healthy.report.ID)
			if err != nil || !reflect.DeepEqual(view, healthy.report) || !bytes.Equal(raw, healthy.html) {
				t.Fatalf("hold disturbed the undamaged historical report: bytes=%d error=%v", len(raw), err)
			}
			if got := snapshotOtherFrozenEvidence(t, b, damaged.report.ID); !reflect.DeepEqual(got, protectedBefore) {
				t.Fatal("read and hold changed protected evidence or report references")
			}
			t.Logf("%s frozen payload refused; assessment and freeze held for both candidates; protected evidence unchanged", damage)
		})
	}
}

type frozenEvidenceBucket struct {
	sequence uint64
	rows     map[string]string
}

func snapshotOtherFrozenEvidence(t *testing.T, b *Board, damagedReport string) map[string]frozenEvidenceBucket {
	t.Helper()
	snapshot := make(map[string]frozenEvidenceBucket)
	if err := b.db.View(func(tx *bolt.Tx) error {
		for _, name := range buckets {
			// The hold and its audit may change, while retained evidence stays exact.
			if name == metaBucket || name == auditBucket || name == auditMetaBucket {
				continue
			}
			bucket := tx.Bucket([]byte(name))
			entry := frozenEvidenceBucket{sequence: bucket.Sequence(), rows: make(map[string]string)}
			if err := bucket.ForEach(func(key, data []byte) error {
				if name != htmlBucket || string(key) != damagedReport {
					entry.rows[string(key)] = string(data)
				}
				return nil
			}); err != nil {
				return err
			}
			snapshot[name] = entry
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
