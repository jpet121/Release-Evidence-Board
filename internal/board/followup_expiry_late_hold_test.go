package board

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupExpiryRechecksHoldBeforeRecordingIntent(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	directory := t.TempDir()
	backup, err := f.app.Backup(secondActor, secondProject, directory)
	if err != nil {
		t.Fatal(err)
	}
	path := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	var app *Board
	var suspendError error
	armed := false
	now := f.at.Add(30 * 24 * time.Hour)
	app, err = Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time {
		if armed {
			// The healthy initial custody read has no AI snapshot. Its
			// next time read is the scan cutoff outside the write.
			armed = false
			suspendError = app.SuspendAssessments(secondActor, secondProject)
		}
		return now
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	armed = true
	count, err := app.ExpireBackups(secondActor, secondProject, directory)
	if armed || suspendError != nil || count != 0 || !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expiry did not observe the committed hold after the first custody read: count=%d, err=%v, suspend=%v", count, err, suspendError)
	}
	events, err := app.Audit(secondActor, secondProject)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(events, func(event AuditEvent) bool {
		return event.Outcome == "success" && (event.Action == "backup.expire_intent" || event.Action == "backup.expire")
	}) {
		t.Fatal("held custody recorded a successful expiry intent or completed an expiry")
	}
	raw, err := os.ReadFile(backup.Path)
	if err != nil || digest(raw) != backup.SHA256 {
		t.Fatalf("held custody changed the verification-bound backup: %v", err)
	}
	if checksum, err := os.ReadFile(backup.Path + ".sha256"); err != nil || string(checksum) != backup.SHA256+"\n" {
		t.Fatalf("held custody changed the exact checksum companion: %v", err)
	}
	if err := app.Revalidate(secondActor, secondProject); err != nil {
		t.Fatal(err)
	}
	if count, err := app.ExpireBackups(secondActor, secondProject, directory); err != nil || count != 1 {
		t.Fatalf("verified healthy retry did not complete the exact eligible pair: count=%d, err=%v", count, err)
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatalf("completed expiry left the backup pair in place: %v %v", entries, err)
	}
	if source, err := app.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(source, f.source) {
		t.Fatalf("expiry changed the verified original upload: %v", err)
	}
	if report, html, err := app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(html, f.html) {
		t.Fatalf("expiry changed the prior frozen decision: %v", err)
	}
}

func TestFollowupSuspensionDoesNotClaimSuccessWhenMarkerCannotWrite(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	key := []byte("assessment_hold")
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.Bucket([]byte(metaBucket)).CreateBucket(key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.SuspendAssessments(secondActor, secondProject); !errors.Is(err, bolt.ErrIncompatibleValue) {
		t.Fatalf("suspension hid the actual incompatible-marker write failure: %v", err)
	}
	events, err := f.app.Audit(secondActor, secondProject)
	if err != nil || slices.ContainsFunc(events, func(event AuditEvent) bool {
		return event.Action == "assessment.suspend" && event.Outcome == "success"
	}) {
		t.Fatalf("failed marker write advertised successful suspension: %v", err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(metaBucket)).DeleteBucket(key)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
		t.Fatalf("reviewed marker repair did not allow real suspension: %v", err)
	}
	reopened := f.checkReopened(t, true)
	if err := reopened.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("reviewer could not clear the genuine durable hold after verification: %v", err)
	}
}
