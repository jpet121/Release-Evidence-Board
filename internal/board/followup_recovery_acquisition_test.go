package board

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupBackupReportsLateNativeTimestampWriteRefusal(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	dir := t.TempDir()
	key := []byte("last_backup")
	type attempt struct {
		backup Backup
		err    error
	}
	finished := make(chan attempt, 1)
	var jobs sync.WaitGroup
	defer jobs.Wait()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	var staged, checksum string
	err := f.app.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(metaBucket))
		if meta.Get(key) != nil || meta.Bucket(key) != nil {
			return errors.New("fixture unexpectedly has a prior saved backup timestamp")
		}
		if _, err := meta.CreateBucket(key); err != nil {
			return err
		}
		// Backup reads the old healthy pages while this write is still
		// uncommitted. Its final timestamp update waits for this writer.
		jobs.Go(func() {
			backup, err := f.app.Backup(secondActor, secondProject, dir)
			finished <- attempt{backup, err}
		})
		poll := time.NewTicker(time.Millisecond)
		defer poll.Stop()
		for {
			select {
			case early := <-finished:
				return errors.Join(errors.New("backup finished before the late timestamp phase"), early.err)
			case <-deadline.C:
				return errors.New("backup did not publish a verified pair before its final update")
			case <-poll.C:
				entries, err := os.ReadDir(dir)
				if err != nil {
					return err
				}
				for _, entry := range entries {
					if !strings.HasSuffix(entry.Name(), ".db.sha256") {
						continue
					}
					companion := filepath.Join(dir, entry.Name())
					sidecar, err := os.ReadFile(companion)
					if err != nil || len(sidecar) != 65 || sidecar[64] != '\n' {
						continue
					}
					snapshot := strings.TrimSuffix(companion, ".sha256")
					raw, err := os.ReadFile(snapshot)
					if err == nil && digest(raw) == string(sidecar[:64]) {
						staged, checksum = snapshot, string(sidecar[:64])
						return nil
					}
				}
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-finished:
		if !errors.Is(result.err, bolt.ErrIncompatibleValue) ||
			result.backup.Path != staged || result.backup.SHA256 != checksum {
			t.Fatalf("backup did not surface the actual late timestamp write refusal after staging: %+v %v", result.backup, result.err)
		}
	case <-deadline.C:
		t.Fatal("backup did not settle after the native staged writer committed")
	}
	if raw, err := os.ReadFile(staged); err != nil || digest(raw) != checksum {
		t.Fatalf("the final metadata refusal altered the prepared backup bytes: %v", err)
	}
	events, err := f.app.Audit(secondActor, secondProject)
	if err != nil || slices.ContainsFunc(events, func(event AuditEvent) bool {
		return event.Action == "backup.create" && event.Outcome == "success"
	}) {
		t.Fatalf("the late metadata refusal advertised a successful backup: %v", err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(metaBucket)).DeleteBucket(key) }); err != nil {
		t.Fatal(err)
	}
	reopened := f.checkReopened(t, false)
	if health, err := reopened.Health(secondActor, secondProject); err != nil || !health.LastBackup.IsZero() {
		t.Fatalf("refused backup transaction left a successful time after exact repair: %+v %v", health.LastBackup, err)
	}
}

func TestFollowupRecoveryReportsNativeTemporaryAcquisitionErrors(t *testing.T) {
	t.Run("restore_parent_missing", func(t *testing.T) {
		f := newIntakeCustodyFixture(t)
		owned := t.TempDir()
		backup, err := f.app.Backup(secondActor, secondProject, owned)
		if err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(owned, "absent-parent", "restored.db")
		result, err := RestoreBackup(backup.Path, destination, Settings{ProjectID: secondProject, ProductID: "DEMO"})
		var native *os.PathError
		// MkdirTemp checks the parent after a missing-path mkdir failure.
		if result.IntegrityOK || !errors.As(err, &native) || !errors.Is(err, os.ErrNotExist) ||
			native.Path != filepath.Dir(destination) {
			t.Fatalf("restore did not report its first native temporary directory creation refusal: %+v %v", result, err)
		}
		if contents, err := os.ReadDir(owned); err != nil || len(contents) != 2 {
			t.Fatalf("failed restore changed the source pair or left new stage state: %v %v", contents, err)
		}
		if data, err := os.ReadFile(backup.Path); err != nil || digest(data) != backup.SHA256 {
			t.Fatalf("failed restore changed the verified source backup: %v", err)
		}
		f.checkReopened(t, false)
	})
	t.Run("backup_path_removed_before_file", func(t *testing.T) {
		f := newIntakeCustodyFixture(t)
		path := f.app.db.Path()
		if err := f.app.Close(); err != nil {
			t.Fatal(err)
		}
		owned := filepath.Join(t.TempDir(), "owned-empty-backups")
		armed := false
		var removeError error
		app, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time {
			if armed {
				armed = false
				removeError = os.Remove(owned)
			}
			return f.at
		}})
		if err != nil {
			t.Fatal(err)
		}
		f.app = app
		t.Cleanup(func() {
			if err := app.Close(); err != nil {
				t.Error(err)
			}
		})
		armed = true
		backup, err := app.Backup(secondActor, secondProject, owned)
		var observed *os.PathError
		if armed || removeError != nil || backup.Path != "" || !errors.As(err, &observed) || observed.Op != "open" ||
			!errors.Is(err, os.ErrNotExist) || filepath.Dir(observed.Path) != owned {
			t.Fatalf("backup did not report temporary file creation against its removed directory: removed=%v backup=%+v err=%v", removeError, backup, err)
		}
		if after, err := os.ReadDir(filepath.Dir(owned)); err != nil || len(after) != 0 {
			t.Fatalf("backup creation refusal left new paths: %v %v", after, err)
		}
		f.checkReopened(t, false)
	})
}
