package board

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

type retainedFailureFixture struct {
	app       *Board
	candidate Candidate
	receipt   Receipt
	source    []byte
	report    Report
	html      []byte
	now       time.Time
}

func followupRetainedFailure(t *testing.T) retainedFailureFixture {
	t.Helper()
	app, baseline, now := reviewBoard(t)
	candidate := reviewCandidate(t, app, baseline, "a")
	receipt, _, source := reviewCommit(t, app, candidate, reviewRows(candidate, "a", "ARCHIVED", "FAIL", now.Add(-time.Hour)))
	reviewCommit(t, app, candidate, reviewRows(candidate, "a", "CURRENT", "PASS", now.Add(-time.Minute)))
	assessment, err := app.Assess(secondActor, secondProject, candidate.ID)
	if err != nil || assessment.Status != "BLOCKED" || len(assessment.Behaviors) != 1 {
		t.Fatalf("fixture did not retain an assessable older failure: %s, %v", assessment.Status, err)
	}
	behavior := assessment.Behaviors[0]
	if behavior.Status != "FAIL" || behavior.CurrentRunID != "CURRENT" || len(behavior.Runs) != 2 {
		t.Fatal("fixture did not distinguish the passing current run and archived blocker")
	}
	report, err := app.Freeze(secondActor, secondProject, candidate.ID)
	if err != nil || report.Status != "BLOCKED" {
		t.Fatalf("fixture report did not capture the historical failure: %v", err)
	}
	_, html, err := app.ReportHTML(secondActor, secondProject, report.ID)
	if err != nil || !bytes.Contains(html, []byte("RATE_BELOW_THRESHOLD")) {
		t.Fatalf("fixture report lacks the older rate blocker: %v", err)
	}
	return retainedFailureFixture{app: app, candidate: candidate, receipt: receipt, source: source, report: report, html: html, now: now}
}

// BR06, AC-007-02 and SPEC-10. The failures are injected after startup into a
// real isolated store. Every decision and artifact is observed using the API.
func TestFollowupLiveHistoricalRunAndReceiptDamageCannotClearFailure(t *testing.T) {
	for _, fault := range []string{
		"indexed run missing", "candidate index redirected", "run and index omitted", "run JSON malformed",
		"receipt row count changed", "receipt run identity changed", "result index redirected",
	} {
		t.Run(fault, func(t *testing.T) {
			fx := followupRetainedFailure(t)
			if err := fx.app.db.Update(func(tx *bolt.Tx) error {
				archived := []byte("ARCHIVED")
				index := tx.Bucket([]byte(candidateRun))
				indexKey := []byte(fx.candidate.ID + "\x00ARCHIVED")
				runs := tx.Bucket([]byte(runBucket))
				switch fault {
				case "indexed run missing":
					return runs.Delete(archived)
				case "candidate index redirected":
					return index.Put(indexKey, []byte("CURRENT"))
				case "run and index omitted":
					if err := index.Delete(indexKey); err != nil {
						return err
					}
					return runs.Delete(archived)
				case "run JSON malformed":
					return runs.Put(archived, []byte("{"))
				case "receipt row count changed":
					receipt := fx.receipt
					receipt.Rows++
					return putJSON(tx, receiptBucket, receipt.Hash, receipt)
				case "receipt run identity changed":
					receipt := fx.receipt
					receipt.AddedRuns = []string{"WRONG-RUN"}
					return putJSON(tx, receiptBucket, receipt.Hash, receipt)
				default:
					return putJSON(tx, resultBucket, "RESARCHIVED-2", "CURRENT")
				}
			}); err != nil {
				t.Fatal(err)
			}
			assessment, err := fx.app.Assess(secondActor, secondProject, fx.candidate.ID)
			if !errors.Is(err, ErrIntegrity) || assessment.Status != "" {
				t.Fatalf("live fault %q produced an assessment %q, error=%v", fault, assessment.Status, err)
			}
			followupCheckHeldArtifacts(t, fx.app, fx)
			t.Logf("AC-007-02 prior=BLOCKED current=CURRENT; SPEC-10 fault=%q assess=integrity freeze=refused frozen_bytes=exact original_csv=exact", fault)
		})
	}
}

// AC-014-02 and AC-011-05. A valid sidecar verifies the copied bytes. The
// restored application's semantic checks still reject bad evidence.
func TestFollowupRestoreRefusesHistoricallyIncompleteCustodyDespiteValidChecksum(t *testing.T) {
	fx := followupRetainedFailure(t)
	backup, err := fx.app.Backup(secondActor, secondProject, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	detached, err := bolt.Open(backup.Path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = detached.Update(func(tx *bolt.Tx) error {
		receipt := fx.receipt
		receipt.LedgerHighWater = 0
		return putJSON(tx, receiptBucket, receipt.Hash, receipt)
	})
	if err := errors.Join(err, detached.Close()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile(backup.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup.Path+".sha256", []byte(fmt.Sprintf("%x\n", sha256.Sum256(snapshot))), 0600); err != nil {
		t.Fatal(err)
	}
	settings := Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return fx.now }}
	destination := filepath.Join(t.TempDir(), "restore.db")
	result, err := RestoreBackup(backup.Path, destination, settings)
	if err != nil || result.IntegrityOK {
		t.Fatalf("restore certified impossible receipt ledger: integrity=%t error=%v", result.IntegrityOK, err)
	}
	restored, err := Open(destination, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("reviewer revalidated the impossible restored ledger: %v", err)
	}
	followupCheckHeldArtifacts(t, restored, fx)
	t.Log("AC-014-02 transport_checksum=valid fault=receipt_highwater_zero restored_integrity=false; AC-011-05 freeze=refused historical_html=exact")
}

func followupCheckHeldArtifacts(t *testing.T, app *Board, fx retainedFailureFixture) {
	t.Helper()
	if held, err := app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
		t.Errorf("detected custody fault did not hold assessments: %t, %v", held, err)
	}
	if report, err := app.Freeze(secondActor, secondProject, fx.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
		t.Errorf("held freeze created a report %q, error=%v", report.ID, err)
	}
	_, preserved, err := app.ReportHTML(secondActor, secondProject, fx.report.ID)
	if err != nil || !bytes.Equal(preserved, fx.html) {
		t.Errorf("existing frozen bytes changed during custody hold: %v", err)
	}
	source, err := app.SourceCSV(secondActor, secondProject, fx.receipt.ID)
	if err != nil || !bytes.Equal(source, fx.source) {
		t.Errorf("verified original CSV bytes could not be recovered: %v", err)
	}
	reports, err := app.Reports(secondActor, secondProject, fx.candidate.ID)
	if err != nil || len(reports) != 1 || reports[0].ID != fx.report.ID {
		t.Errorf("custody fault changed the report inventory: %v", err)
	}
}
