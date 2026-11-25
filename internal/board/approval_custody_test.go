package board

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func independentlyApprovedReport(t *testing.T) (*Board, ApprovedBaseline, Report, []byte, time.Time) {
	t.Helper()
	b, prior, draft, at := retainedHighMarkAndReducedDraft(t)
	approval, err := b.Approve(draftAuthor, secondProject, draft.ID, draft.Revision)
	if err != nil || approval.Approver != draftAuthor || len(approval.Thresholds) != 1 {
		t.Fatalf("fixture lacks verified independent D20 approval: %v", err)
	}
	report := freezeHalfPassingSuccessor(t, b, prior, approval, at)
	_, html, err := b.ReportHTML(secondActor, secondProject, report.ID)
	if err != nil || !bytes.Contains(html, []byte("Independent threshold approvals")) ||
		!bytes.Contains(html, []byte("10000 to 5000 basis points")) ||
		!bytes.Contains(html, []byte("approved by reviewer-c at")) {
		t.Fatalf("intact frozen provenance omitted the actual independent approval: %v", err)
	}
	return b, approval, report, html, at
}

func TestApprovedBaselineReadRejectsMissingChangedAndOrphanedBindings(t *testing.T) {
	for _, fault := range []string{"missing binding", "wrong binding", "orphaned binding"} {
		t.Run(fault, func(t *testing.T) {
			b, approved, _ := reviewBoard(t)
			key := []byte(approved.Descriptor.Digest)
			if err := b.db.Update(func(tx *bolt.Tx) error {
				metadata := tx.Bucket([]byte(baselineMetaBucket))
				switch fault {
				case "missing binding":
					return metadata.Delete(key)
				case "wrong binding":
					return metadata.Put(key, []byte(digest([]byte("other approval"))))
				default:
					return tx.Bucket([]byte(baselineBucket)).Delete(key)
				}
			}); err != nil {
				t.Fatal(err)
			}
			if result, err := b.Baseline(secondActor, secondProject, string(key)); !errors.Is(err, ErrIntegrity) || result.Descriptor.Digest != "" {
				t.Errorf("unverified approval read returned metadata: %q, %v", result.Descriptor.Digest, err)
			}
			if _, _, err := b.Baselines(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
				t.Errorf("catalog accepted invalid approval binding without any candidates: %v", err)
			}
			if err := b.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
				t.Errorf("full validation accepted invalid approval binding: %v", err)
			}
		})
	}
}

func TestApprovedBaselineCorruptionBlocksNewAssessmentAndDerivation(t *testing.T) {
	for _, operation := range []string{"assess", "candidate lookup", "draft derivation"} {
		t.Run(operation, func(t *testing.T) {
			b, approval, report, _, _ := independentlyApprovedReport(t)
			approval.Thresholds = nil
			if err := b.db.Update(func(tx *bolt.Tx) error {
				return putJSON(tx, baselineBucket, approval.Descriptor.Digest, approval)
			}); err != nil {
				t.Fatal(err)
			}
			var err error
			switch operation {
			case "assess":
				_, err = b.Assess(secondActor, secondProject, report.CandidateID)
			case "candidate lookup":
				_, _, _, err = b.Candidate(secondActor, secondProject, report.CandidateID)
			default:
				_, err = b.CreateDraft(secondActor, secondProject, approval.Baseline, DraftOptions{FromBaseline: approval.Descriptor.Digest})
			}
			if !errors.Is(err, ErrIntegrity) {
				t.Errorf("%s consumed unverified approval metadata: %v", operation, err)
			}
			if held, err := b.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
				t.Errorf("%s failed to hold new assessments: %t, %v", operation, held, err)
			}
		})
	}
}

func TestApprovedBaselineRestoreChecksProvenanceSeparatelyFromChecksum(t *testing.T) {
	for _, fault := range []string{"replaced approver", "legacy without baseline bindings"} {
		t.Run(fault, func(t *testing.T) {
			b, approval, report, original, at := independentlyApprovedReport(t)
			backup, err := b.Backup(secondActor, secondProject, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := bolt.Open(backup.Path, 0600, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = snapshot.Update(func(tx *bolt.Tx) error {
				if fault == "legacy without baseline bindings" {
					return tx.DeleteBucket([]byte(baselineMetaBucket))
				}
				approval.Approver = secondActor
				return putJSON(tx, baselineBucket, approval.Descriptor.Digest, approval)
			})
			if err := errors.Join(err, snapshot.Close()); err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(backup.Path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(backup.Path+".sha256", []byte(digest(contents)+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "restore.db")
			settings := Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return at }}
			recovery, err := RestoreBackup(backup.Path, path, settings)
			if err != nil || recovery.IntegrityOK {
				t.Fatalf("recovery certified damaged or unbound approval metadata: %t, %v", recovery.IntegrityOK, err)
			}
			restored, err := Open(path, settings)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if err := restored.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
				t.Errorf("restored approval provenance incorrectly cleared hold: %v", err)
			}
			if held, err := restored.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
				t.Errorf("restored approval provenance did not hold live decisions: %t, %v", held, err)
			}
			_, html, err := restored.ReportHTML(secondActor, secondProject, report.ID)
			if err != nil || !bytes.Equal(html, original) {
				t.Errorf("held restore changed valid frozen bytes: %v", err)
			}
		})
	}
}

func TestApprovedBaselineCorruptionCannotMisstateFrozenD20Approval(t *testing.T) {
	for _, fault := range []string{"missing thresholds", "replaced approver"} {
		t.Run(fault, func(t *testing.T) {
			b, approval, originalReport, originalHTML, _ := independentlyApprovedReport(t)
			if fault == "missing thresholds" {
				approval.Thresholds = nil
			} else {
				approval.Approver = secondActor
			}
			if err := b.db.Update(func(tx *bolt.Tx) error {
				return putJSON(tx, baselineBucket, approval.Descriptor.Digest, approval)
			}); err != nil {
				t.Fatal(err)
			}
			if err := b.db.View(validateStore); !errors.Is(err, ErrIntegrity) {
				t.Errorf("full integrity check accepted changed approval metadata: %v", err)
			}
			newReport, err := b.Freeze(secondActor, secondProject, originalReport.CandidateID)
			if err == nil {
				_, html, readErr := b.ReportHTML(secondActor, secondProject, newReport.ID)
				t.Errorf("unverified approval emitted a %s report: threshold history=%t, false approver=%t, read=%v",
					newReport.Status, bytes.Contains(html, []byte("Independent threshold approvals")),
					bytes.Contains(html, []byte("approved by reviewer at")), readErr)
			} else if !errors.Is(err, ErrIntegrity) || newReport.ID != "" {
				t.Errorf("fresh freeze did not reject changed approval metadata: %v", err)
			}
			if held, err := b.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
				t.Errorf("approval metadata fault did not suspend assessments: %t, %v", held, err)
			}
			if err := b.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
				t.Errorf("reviewer revalidated corrupted approval history: %v", err)
			}
			_, html, err := b.ReportHTML(secondActor, secondProject, originalReport.ID)
			if err != nil || !bytes.Equal(originalHTML, html) {
				t.Errorf("already frozen valid provenance was not preserved: %v", err)
			}
		})
	}
}
