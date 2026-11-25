package board

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupExpiryOnlyResumesOwnedDirectoryIntents(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	path := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	now := f.at
	app, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	first, second := t.TempDir(), t.TempDir()
	a, err := app.Backup(secondActor, secondProject, first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := app.Backup(secondActor, secondProject, second)
	if err != nil {
		t.Fatal(err)
	}
	companion := a.Path + ".sha256"
	separate := t.TempDir()
	if err := os.Rename(companion, filepath.Join(separate, "saved.sha256")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(companion, 0700); err != nil {
		t.Fatal(err)
	}
	contents := []byte("private unrelated entry")
	entry := filepath.Join(companion, "occupied")
	if err := os.WriteFile(entry, contents, 0600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * 24 * time.Hour)
	if count, err := app.ExpireBackups(secondActor, secondProject, first); err == nil || count != 0 {
		t.Fatalf("fixture did not stop after durable intent: %d, %v", count, err)
	}
	if _, err := os.Stat(a.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture did not reach partial removal: %v", err)
	}
	if count, err := app.ExpireBackups(secondActor, secondProject, second); err != nil || count != 1 {
		t.Errorf("unrelated pending intent interfered with the selected directory: %d, %v", count, err)
	}
	if _, err := os.Stat(b.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("eligible selected pair was not removed: %v", err)
	}
	if raw, err := os.ReadFile(entry); err != nil || !bytes.Equal(raw, contents) {
		t.Fatalf("expiry of the second directory changed the pending first directory: %v", err)
	}
	if err := os.Rename(companion, filepath.Join(separate, "saved-occupied")); err != nil {
		t.Fatal(err)
	}
	if count, err := app.ExpireBackups(secondActor, secondProject, first); err != nil || count != 1 {
		t.Errorf("exact selected retry did not complete its own intent: %d, %v", count, err)
	}
	if raw, err := app.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(raw, f.source) {
		t.Errorf("directory-scoped expiry changed the independent original: %v", err)
	}
}

func TestFollowupDrillRefusesNestedLiveTimestampUntilExactRepair(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	backup, err := f.app.Backup(secondActor, secondProject, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "checked-restore.db")
	settings := Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return f.at }}
	if restored, err := RestoreBackup(backup.Path, snapshot, settings); err != nil || !restored.IntegrityOK {
		t.Fatalf("fixture did not produce a separately verified restored copy: %v", err)
	}
	key := []byte("last_drill")
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.Bucket([]byte(metaBucket)).CreateBucket(key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.RecordRestoreDrill(secondActor, secondProject, snapshot); !errors.Is(err, bolt.ErrIncompatibleValue) {
		t.Errorf("drill recorded success over a nested live timestamp: %v", err)
	}
	if _, err := f.app.Health(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
		t.Errorf("operator health treated nested saved timestamp as missing: %v", err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
		t.Errorf("reviewer cleared the unresolved native timestamp fault: %v", err)
	}
	if events, err := f.app.Audit(secondActor, secondProject); err != nil || slices.ContainsFunc(events, func(event AuditEvent) bool {
		return event.Action == "backup.drill" && event.Outcome == "success"
	}) {
		t.Errorf("failed drill added a successful audit event: %v", err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(metaBucket)).DeleteBucket(key) }); err != nil {
		t.Fatal(err)
	}
	app := f.checkReopened(t, true)
	if err := app.RecordRestoreDrill(secondActor, secondProject, snapshot); err != nil {
		t.Fatal(err)
	}
	if h, err := app.Health(secondActor, secondProject); err != nil || !h.LastDrill.Equal(f.at) || h.Hold {
		t.Errorf("exact repair did not recover a normal health timestamp: %v", err)
	}
}
