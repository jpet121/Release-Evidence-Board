package board_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"release-evidence-board/internal/board"
)

func TestFollowupRestoreRequiresExistingBoardIdentity(t *testing.T) {
	original, _ := openBoard(t, func() time.Time { return fixtureTime })
	candidates, err := original.Candidates(supervisor, project)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("initialized Board fixture must have zero candidates: %v", err)
	}
	backup, err := original.Backup(supervisor, project, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(backup.Path)
	if err != nil || custodyDigest(raw) != backup.SHA256 {
		t.Fatalf("initialized backup fixture bytes differ: %v", err)
	}
	t.Cleanup(func() { assertRestoreSourceUnchanged(t, backup) })
	metadata := func(key, value string) func(*bolt.Tx) error {
		return func(tx *bolt.Tx) error {
			return tx.Bucket([]byte("meta")).Put([]byte(key), []byte(value))
		}
	}
	for _, trial := range []struct {
		name         string
		before       []byte
		bucketless   bool
		edit         func(*bolt.Tx) error
		wantVerified bool
	}{
		{name: "zero-bytes"},
		{name: "bucketless-bbolt", bucketless: true},
		{name: "invalid-header", before: bytes.Repeat([]byte("invalid synthetic header\n"), 1024)},
		{name: "missing-metadata", before: raw, edit: func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte("meta")) }},
		{name: "foreign-project", before: raw, edit: metadata("project", "THEREMOTE")},
		{name: "foreign-product", before: raw, edit: metadata("product", "OTHER")},
		{name: "unsupported-schema", before: raw, edit: metadata("schema", "reb-store/99")},
		{name: "initialized-zero-candidate-board", before: raw, wantVerified: true},
		{name: "recognized-legacy-board", before: raw, edit: func(tx *bolt.Tx) error {
			return tx.DeleteBucket([]byte("backup_expiry"))
		}, wantVerified: true},
	} {
		t.Run(trial.name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.db")
			if err := os.WriteFile(source, trial.before, 0600); err != nil {
				t.Fatal(err)
			}
			if trial.bucketless || trial.edit != nil {
				db, err := bolt.Open(source, 0600, nil)
				if err != nil {
					t.Fatal(err)
				}
				if trial.bucketless {
					err = db.View(func(tx *bolt.Tx) error {
						return tx.ForEach(func(_ []byte, _ *bolt.Bucket) error {
							return errors.New("native empty bbolt fixture already contains a bucket")
						})
					})
				} else {
					err = db.Update(trial.edit)
				}
				if err := errors.Join(err, db.Close()); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			identity := board.Backup{Path: source, SHA256: custodyDigest(before)}
			if err := os.WriteFile(source+".sha256", []byte(identity.SHA256+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { assertRestoreSourceUnchanged(t, identity) })
			destination := filepath.Join(dir, "restored.db")
			settings := componentClock(fixtureTime)
			result, restoreErr := board.RestoreBackup(source, destination, settings)
			_, statErr := os.Lstat(destination)
			if !trial.wantVerified {
				if restoreErr == nil || result.IntegrityOK || !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("unrecognized backup was accepted: source_bytes=%d, verified=%t, restore_err=%v, destination_stat=%v",
						len(before), result.IntegrityOK, restoreErr, statErr)
				}
				assertRestoreDirectorySize(t, dir, 2)
				t.Logf("rejected unrecognized source of %d bytes before publication: %v", len(before), restoreErr)
				return
			}
			if restoreErr != nil || !result.IntegrityOK || statErr != nil {
				t.Fatalf("recognized Board could not be restored: verified=%t, restore_err=%v, destination_stat=%v", result.IntegrityOK, restoreErr, statErr)
			}
			assertRestoreDirectorySize(t, dir, 3)
			restored, err := board.Open(destination, settings)
			if err != nil {
				t.Fatal(err)
			}
			defer componentClose(t, restored)
			held, err := restored.AssessmentSuspended(supervisor, project)
			if err != nil || !held {
				t.Fatalf("verified restoration did not await reviewer revalidation: %t, %v", held, err)
			}
			retained, err := restored.Candidates(supervisor, project)
			if err != nil || len(retained) != 0 {
				t.Fatalf("verified restore did not preserve the empty candidate roster: %v", err)
			}
			t.Log("recognized zero-candidate Board restored with a reviewer hold")
		})
	}

	t.Run("occupied-destination", func(t *testing.T) {
		dir := t.TempDir()
		destination := filepath.Join(dir, "existing.db")
		sentinel := []byte("other owner's destination")
		if err := os.WriteFile(destination, sentinel, 0600); err != nil {
			t.Fatal(err)
		}
		result, err := board.RestoreBackup(backup.Path, destination, componentClock(fixtureTime))
		if !errors.Is(err, os.ErrExist) || result.IntegrityOK {
			t.Fatalf("occupied destination was accepted: verified=%t, %v", result.IntegrityOK, err)
		}
		actual, err := os.ReadFile(destination)
		if err != nil || !bytes.Equal(actual, sentinel) {
			t.Fatalf("restore changed another owner's destination: %v", err)
		}
		assertRestoreDirectorySize(t, dir, 1)
	})
}

func assertRestoreDirectorySize(t *testing.T, dir string, entries int) {
	t.Helper()
	actual, err := os.ReadDir(dir)
	if err != nil || len(actual) != entries {
		t.Fatalf("restore left unexpected files: %v %v", actual, err)
	}
}
