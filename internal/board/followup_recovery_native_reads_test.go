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

func TestFollowupRestoreCopyRefusesDirectorySourceAndCleansStage(t *testing.T) {
	root := t.TempDir()
	before := []byte("independent retained artifact")
	retained := filepath.Join(root, "previous.html")
	if err := os.WriteFile(retained, before, 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "directory-source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "new.db")
	result, err := RestoreBackup(source, destination, Settings{ProjectID: secondProject, ProductID: "DEMO"})
	var pathErr *os.PathError
	if result.IntegrityOK || !errors.As(err, &pathErr) || pathErr.Op != "read" || pathErr.Path != source {
		t.Errorf("restore did not surface the actual directory read failure: %+v, %v", result, err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("failed copy published a restore target: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 || entries[0].Name() != filepath.Base(source) || entries[1].Name() != filepath.Base(retained) {
		t.Errorf("failed copy left staging files or changed other entries: %v, %v", entries, err)
	}
	if after, err := os.ReadFile(retained); err != nil || !bytes.Equal(after, before) {
		t.Errorf("failed copy changed the independent retained bytes: %v", err)
	}
}

func TestFollowupRestorePropagatesNativeDestinationStatusFailure(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	directory := t.TempDir()
	backup, err := f.app.Backup(secondActor, secondProject, directory)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "invalid\x00destination.db")
	_, lookupError := os.Lstat(destination)
	var native *os.PathError
	if !errors.As(lookupError, &native) || errors.Is(lookupError, os.ErrNotExist) || native.Path != destination {
		t.Fatalf("native fixture did not produce a destination status error: %v", lookupError)
	}
	result, err := RestoreBackup(backup.Path, destination, Settings{ProjectID: secondProject, ProductID: "DEMO"})
	var observed *os.PathError
	if result.IntegrityOK || !result.RestoredAt.IsZero() || !errors.As(err, &observed) ||
		observed.Path != native.Path || observed.Op != native.Op || !errors.Is(err, native.Err) {
		t.Fatalf("restore hid its first native destination status error: %+v %v, native=%v", result, err, lookupError)
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 2 {
		t.Fatalf("destination status refusal left a stage or changed the backup pair: %v %v", entries, err)
	}
	if raw, err := os.ReadFile(backup.Path); err != nil || digest(raw) != backup.SHA256 {
		t.Fatalf("destination status refusal changed the verified snapshot: %v", err)
	}
	if sidecar, err := os.ReadFile(backup.Path + ".sha256"); err != nil || string(sidecar) != backup.SHA256+"\n" {
		t.Fatalf("destination status refusal changed the checksum companion: %v", err)
	}
	f.checkReopened(t, false)
}

func TestFollowupBackupCleansStageAfterStoreClosesBeforeSnapshot(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	path := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	var app *Board
	var closeErr error
	armed := false
	settings := Settings{
		ProjectID: secondProject,
		ProductID: "DEMO",
		Clock: func() time.Time {
			if armed {
				armed = false
				closeErr = app.Close()
			}
			return f.at
		},
	}
	var err error
	app, err = Open(path, settings)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	armed = true
	backup, err := app.Backup(secondActor, secondProject, directory)
	if armed || closeErr != nil {
		t.Fatalf("did not close the application between authorization and reading: %v", closeErr)
	}
	if backup.Path != "" || !errors.Is(err, bolt.ErrDatabaseNotOpen) {
		t.Errorf("backup did not retain the actual closed-store read failure: %+v, %v", backup, err)
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Errorf("failed view left an owned staging or publication file: %v, %v", entries, err)
	}
	reopened, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return f.at }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	if csv, err := reopened.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(csv, f.source) {
		t.Errorf("reopened evidence no longer has the exact prior source: %v", err)
	}
	if report, html, err := reopened.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(html, f.html) {
		t.Errorf("reopened evidence no longer has the exact prior report: %v", err)
	}
	if verified, err := reopened.Backup(secondActor, secondProject, directory); err != nil || verified.Path == "" {
		t.Errorf("reopened healthy store could not create a verified snapshot: %v", err)
	}
}
