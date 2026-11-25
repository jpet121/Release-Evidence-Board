package board

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupRestoreRequiresRecordedReducingActor(t *testing.T) {
	checkRestoredReducingActorShape(t, nil)
}

func TestFollowupRestoreRejectsMalformedReducingActorShape(t *testing.T) {
	for _, entry := range []struct {
		name   string
		actors []string
	}{
		{"empty array", []string{}},
		{"blank actor", []string{""}},
		{"invalid actor identifier", []string{"not an actor ID"}},
		{"duplicate historical actor", []string{secondActor, secondActor}},
	} {
		t.Run(entry.name, func(t *testing.T) { checkRestoredReducingActorShape(t, entry.actors) })
	}
}

func TestFollowupRestoreRefusesRecordedApproverAmongReducers(t *testing.T) {
	t.Run("approver only", func(t *testing.T) { checkRestoredReducingActorShape(t, []string{draftAuthor}) })
	t.Run("independent reducer and approver", func(t *testing.T) {
		checkRestoredReducingActorShape(t, []string{secondActor, draftAuthor})
	})
}

func TestFollowupRestoreKeepsValidRemovedHistoricalReducer(t *testing.T) {
	app, approval, prior, original, at := independentlyApprovedReport(t)
	const custodian = "synthetic-custodian"
	if err := app.SetAccount(secondActor, custodian, "synthetic-custodian-fixture", []Role{Operator, Reviewer}); err != nil {
		t.Fatal(err)
	}
	if err := app.RemoveAccount(custodian, secondActor); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Account(secondActor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("historical reducing account was not removed: %v", err)
	}
	backup, err := app.Backup(custodian, secondProject, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	settings := Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return at }}
	path := filepath.Join(t.TempDir(), "restored.db")
	if restored, err := RestoreBackup(backup.Path, path, settings); err != nil || !restored.IntegrityOK {
		t.Fatalf("valid historical actor without live account blocked verified restore: %v", err)
	}
	app, err = Open(path, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := app.Revalidate(custodian, secondProject); err != nil {
		t.Fatal(err)
	}
	a, err := app.Assess(custodian, secondProject, prior.CandidateID)
	if err != nil || a.Status != Sufficient || len(a.Baseline.Thresholds) != 1 ||
		!slices.Equal(a.Baseline.Thresholds[0].ReducingActors, approval.Thresholds[0].ReducingActors) {
		t.Errorf("revalidation lost the independent recorded reducer: %q, %v", a.Status, err)
	}
	if report, html, err := app.ReportHTML(custodian, secondProject, prior.ID); err != nil || report != prior || !bytes.Equal(html, original) {
		t.Errorf("restore rewrote the prior verified D20 history: %v", err)
	}
}

func checkRestoredReducingActorShape(t *testing.T, actors []string) {
	t.Helper()
	app, approval, prior, original, at := independentlyApprovedReport(t)
	if !slices.Equal(approval.Thresholds[0].ReducingActors, []string{secondActor}) || approval.Approver == secondActor || prior.Status != Sufficient {
		t.Fatal("fixture lacks a proven independent reduction and successor")
	}
	backup, err := app.Backup(secondActor, secondProject, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	settings := Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return at }}
	control, err := RestoreBackup(backup.Path, filepath.Join(t.TempDir(), "healthy.db"), settings)
	if err != nil || !control.IntegrityOK {
		t.Fatalf("original backed-up reduction did not verify: %v", err)
	}
	damaged, err := bolt.Open(backup.Path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	approval.Thresholds[0].ReducingActors = actors
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
	target := filepath.Join(t.TempDir(), "held.db")
	result, err := RestoreBackup(backup.Path, target, settings)
	if err != nil {
		t.Fatal(err)
	}
	if result.IntegrityOK {
		t.Error("restored approval verified despite its missing reducing actor")
	}
	restored, err := Open(target, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restored.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := restored.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
		t.Errorf("reviewer accepted the restored reduction with no reducing actor: %v", err)
	}
	if assessed, err := restored.Assess(secondActor, secondProject, prior.CandidateID); !errors.Is(err, ErrIntegrity) || assessed.Status != "" {
		t.Errorf("unverified reduction produced a restored assessment %q: %v", assessed.Status, err)
	}
	if report, err := restored.Freeze(secondActor, secondProject, prior.CandidateID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
		t.Errorf("unverified reduction produced a new restored report %q: %v", report.Status, err)
	}
	if read, raw, err := restored.ReportHTML(secondActor, secondProject, prior.ID); err != nil || read != prior || !bytes.Equal(raw, original) {
		t.Errorf("recovery changed the independently verified historical report: %v", err)
	}
}
