package board

import (
	"bytes"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupReportLedgerWitnessKeepsReviewerHold(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	_, _, baseline, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	now := f.at.Add(24 * time.Hour)
	app, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	retained := reviewCandidate(t, app, baseline, "b")
	before, _, beforeRaw := reviewCommit(t, app, retained, reviewRows(retained, "b", "RETAINED", Pass, now))
	later, _, _ := reviewCommit(t, app, f.candidate, reviewRows(f.candidate, "a", "EXPIRING", Pass, now))
	report, err := app.Freeze(secondActor, secondProject, retained.ID)
	if err != nil || before.LedgerHighWater != 2 || later.LedgerHighWater != 3 || report.LedgerHighWater != 3 {
		t.Fatalf("fixture did not freeze the retained candidate at the product ledger: %v", err)
	}
	_, html, err := app.ReportHTML(secondActor, secondProject, report.ID)
	if err != nil {
		t.Fatal(err)
	}
	now = f.at.Add(180*24*time.Hour + time.Hour)
	ids := []string{f.candidate.ID}
	purge, err := app.PreviewPurge(secondActor, secondProject, ids)
	if err != nil || len(purge.Ineligible) != 0 || len(purge.SharedBlockers) != 0 {
		t.Fatalf("fixture did not isolate the expiring receipt owner: %v", err)
	}
	if _, err := app.Purge(secondActor, secondProject, ids, purge.Confirmation); err != nil {
		t.Fatal(err)
	}
	for _, receipt := range []Receipt{f.receipt, later} {
		if _, err := app.Receipt(secondActor, secondProject, receipt.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expired receipt still supplies a ledger witness: %v", err)
		}
	}
	if err := app.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("valid purged history did not revalidate before the counter fault: %v", err)
	}
	if err := app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(metaBucket)).Put([]byte("ledger"), []byte("2"))
	}); err != nil {
		t.Fatal(err)
	}
	raw := []byte(strings.Join(resultColumns[:], ",") + "\n" + reviewRows(retained, "b", "AFTER-HOLD", Pass, now))
	if preview, err := app.PreviewImport(secondActor, secondProject, retained.ID, raw); !errors.Is(err, ErrIntegrity) || preview.Token != "" {
		t.Fatalf("import did not observe the retained report's later ledger: %v", err)
	}
	if held, err := app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
		t.Fatalf("import validation did not set a hold: %v", err)
	}
	if err := app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
		t.Errorf("reviewer cleared report-ledger hold without allocator repair: %v", err)
	}
	if held, err := app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
		t.Errorf("unrepaired report ledger no longer holds assessments: %v", err)
	}
	if retainedReport, body, err := app.ReportHTML(secondActor, secondProject, report.ID); err != nil ||
		retainedReport != report || !bytes.Equal(body, html) {
		t.Errorf("ledger validation changed the prior frozen artifact: %v", err)
	}
	if original, err := app.SourceCSV(secondActor, secondProject, before.ID); err != nil || !bytes.Equal(original, beforeRaw) {
		t.Errorf("ledger validation changed the remaining source CSV: %v", err)
	}
	if err := app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(metaBucket)).Put([]byte("ledger"), []byte("3"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("reviewer could not clear the hold after repair: %v", err)
	}
	preview, err := app.PreviewImport(secondActor, secondProject, retained.ID, raw)
	if err != nil || preview.Token == "" {
		t.Fatalf("repair did not recover normal preview: %v", err)
	}
	receipt, err := app.CommitImport(secondActor, secondProject, preview.Token, nil)
	if err != nil || receipt.LedgerHighWater != 4 {
		t.Fatalf("new allocation lost the previously frozen watermark: %v", err)
	}
	if replay, err := app.CommitImport(secondActor, secondProject, preview.Token, nil); err != nil || !reflect.DeepEqual(replay, receipt) {
		t.Errorf("exact committed replay changed after repair: %v", err)
	}
}

func TestFollowupWrongStoredHoldTypeKeepsAssessmentsSuspended(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	if held, err := f.app.AssessmentSuspended(secondActor, secondProject); err != nil || held {
		t.Fatalf("the empty hold slot did not allow ordinary assessments: %v", err)
	}
	if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
		t.Fatal(err)
	}
	key := []byte("assessment_hold")
	var saved []byte
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(metaBucket))
		saved = bytes.Clone(meta.Get(key))
		if len(saved) == 0 {
			return errors.New("fixture did not persist an assessment hold")
		}
		if err := meta.Delete(key); err != nil {
			return err
		}
		_, err := meta.CreateBucket(key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if held, err := f.app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
		t.Errorf("invalid saved hold type allowed active assessments: %v", err)
	}
	if assessment, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || assessment.Status != "" {
		t.Errorf("invalid saved hold type allowed assessment %q: %v", assessment.Status, err)
	}
	if report, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
		t.Errorf("invalid saved hold type allowed frozen report %q: %v", report.ID, err)
	}
	if report, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil ||
		report != f.report || !bytes.Equal(html, f.html) {
		t.Errorf("damaged hold type changed historical report bytes: %v", err)
	}
	settings := Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return f.at }}
	backup, err := f.app.Backup(secondActor, secondProject, t.TempDir())
	if !errors.Is(err, ErrIntegrity) || backup.Path != "" {
		t.Errorf("backup verified a snapshot with an invalid stored hold type: %v", err)
	}
	if backup.Path != "" {
		if _, err := RestoreBackup(backup.Path, filepath.Join(t.TempDir(), "restore.db"), settings); errors.Is(err, bolt.ErrIncompatibleValue) {
			t.Log("the resulting snapshot failed restore on the same stored hold type")
		}
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(metaBucket))
		if err := meta.DeleteBucket(key); err != nil {
			return err
		}
		return meta.Put(key, saved)
	}); err != nil {
		t.Fatal(err)
	}
	backup, err = f.app.Backup(secondActor, secondProject, t.TempDir())
	if err != nil || backup.Path == "" {
		t.Fatalf("correctly stored suspension prevented a verified backup: %v", err)
	}
	if restored, err := RestoreBackup(backup.Path, filepath.Join(t.TempDir(), "restore.db"), settings); err != nil || !restored.IntegrityOK {
		t.Fatalf("valid suspended snapshot could not be verified during restore: %v", err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("reviewer could not recover from the corrected hold type: %v", err)
	}
	if a, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); err != nil || a.Status != Sufficient {
		t.Errorf("cleared repaired hold did not recover the prior assessment: %q, %v", a.Status, err)
	}
}
