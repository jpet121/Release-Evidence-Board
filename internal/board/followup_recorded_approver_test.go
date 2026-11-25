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

func TestFollowupRestoreRequiresAWellFormedRecordedReviewer(t *testing.T) {
	for _, variant := range []struct {
		name, recorded string
		reduction      bool
	}{
		{"ordinary missing", "", false},
		{"ordinary malformed", "not an account ID", false},
		{"reduction missing", "", true},
		{"reduction malformed", "not an account ID", true},
	} {
		t.Run(variant.name, func(t *testing.T) {
			var app *Board
			var approval ApprovedBaseline
			var prior Report
			var original []byte
			var at time.Time
			if variant.reduction {
				app, approval, prior, original, at = independentlyApprovedReport(t)
			} else {
				f := newIntakeCustodyFixture(t)
				app, prior, original, at = f.app, f.report, f.html, f.at
				var err error
				_, _, approval, err = app.Candidate(secondActor, secondProject, f.candidate.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if approval.Approver == "" || (len(approval.Thresholds) != 0) != variant.reduction || prior.Status != Sufficient {
				t.Fatal("did not obtain an independently verified ordinary or reduction approval")
			}
			backup, err := app.Backup(secondActor, secondProject, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			damaged, err := bolt.Open(backup.Path, 0600, nil)
			if err != nil {
				t.Fatal(err)
			}
			approval.Approver = variant.recorded
			err = damaged.Update(func(tx *bolt.Tx) error {
				return putBoundJSON(tx, baselineBucket, baselineMetaBucket, approval.Descriptor.Digest, approval)
			})
			if err := errors.Join(err, damaged.Close()); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(backup.Path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(backup.Path+".sha256", []byte(fmt.Sprintf("%x\n", sha256.Sum256(data))), 0600); err != nil {
				t.Fatal(err)
			}
			restored, result := restoreApprovalReadback(t, backup.Path, at)
			if result.IntegrityOK {
				t.Error("backup with no well-formed recorded reviewer verified")
			}
			if err := restored.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
				t.Errorf("reviewer cleared missing or malformed approval identity: %v", err)
			}
			if a, err := restored.Assess(secondActor, secondProject, prior.CandidateID); !errors.Is(err, ErrIntegrity) || a.Status != "" {
				t.Errorf("unverified reviewer produced restored assessment %q: %v", a.Status, err)
			}
			if r, err := restored.Freeze(secondActor, secondProject, prior.CandidateID); !errors.Is(err, ErrIntegrity) || r.ID != "" {
				t.Errorf("unverified reviewer produced new frozen assessment %q: %v", r.Status, err)
			}
			if r, html, err := restored.ReportHTML(secondActor, secondProject, prior.ID); err != nil || r != prior || !bytes.Equal(html, original) {
				t.Errorf("verifying recorded reviewer changed independently frozen bytes: %v", err)
			}
		})
	}
}

func TestFollowupRestoreKeepsValidRemovedHistoricalApprover(t *testing.T) {
	app, approval, prior, original, at := independentlyApprovedReport(t)
	if err := app.RemoveAccount(secondActor, approval.Approver); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Account(approval.Approver); !errors.Is(err, ErrNotFound) {
		t.Fatalf("historical approving account was not removed: %v", err)
	}
	backup, err := app.Backup(secondActor, secondProject, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	restored, result := restoreApprovalReadback(t, backup.Path, at)
	if !result.IntegrityOK {
		t.Fatal("historical approval depended on a current account")
	}
	if err := restored.Revalidate(secondActor, secondProject); err != nil {
		t.Fatal(err)
	}
	if a, err := restored.Assess(secondActor, secondProject, prior.CandidateID); err != nil || a.Status != Sufficient || a.Baseline.Approver != approval.Approver {
		t.Errorf("valid historical approving reviewer was lost: %q, %v", a.Status, err)
	}
	if r, raw, err := restored.ReportHTML(secondActor, secondProject, prior.ID); err != nil || r != prior || !bytes.Equal(raw, original) {
		t.Errorf("historical reviewer removal changed prior frozen bytes: %v", err)
	}
}

func restoreApprovalReadback(t *testing.T, source string, at time.Time) (*Board, RestoreResult) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "restored.db")
	settings := Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return at }}
	result, err := RestoreBackup(source, path, settings)
	if err != nil {
		t.Fatal(err)
	}
	app, err := Open(path, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	return app, result
}
