package board

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupExpiryRefusesIndependentlyInvalidJournalShapes(t *testing.T) {
	for _, kind := range []string{"invalid directory type", "invalid duration type", "relative directory", "noncanonical directory", "invalid backup name"} {
		t.Run(kind, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			dir := t.TempDir()
			backup, err := f.app.Backup(secondActor, secondProject, dir)
			if err != nil {
				t.Fatal(err)
			}
			intent := backupExpiry{Directory: dir, Name: filepath.Base(backup.Path), MinimumAge: defaultBackupExpiry}
			switch kind {
			case "relative directory":
				intent.Directory = "relative-custody"
			case "noncanonical directory":
				intent.Directory += string(filepath.Separator) + "."
			case "invalid backup name":
				intent.Name += "-unrecognized"
			}
			raw, err := json.Marshal(intent)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "invalid directory type" || kind == "invalid duration type" {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				if kind == "invalid directory type" {
					fields["Directory"] = json.RawMessage("7")
				} else {
					fields["MinimumAge"] = json.RawMessage(`"7 hours"`)
				}
				raw, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
			}
			key := []byte(intent.key())
			if err := f.app.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(expiryBucket)).Put(key, raw) }); err != nil {
				t.Fatal(err)
			}
			if count, err := f.app.ExpireBackups(secondActor, secondProject, dir); count != 0 || !errors.Is(err, ErrIntegrity) {
				t.Errorf("unverified backup journal allowed expiry: %d, %v", count, err)
			}
			if _, err := os.Stat(backup.Path + ".sha256"); err != nil {
				t.Errorf("invalid journal changed the real backup companion: %v", err)
			}
			if raw, err := os.ReadFile(backup.Path); err != nil || digest(raw) != backup.SHA256 {
				t.Errorf("invalid journal changed the verified backup bytes: %v", err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(expiryBucket)).Delete(key) }); err != nil {
				t.Fatal(err)
			}
			f.checkReopened(t, true)
		})
	}
}

func TestFollowupExpiryPropagatesNativeInvalidPathBeforeRemovingFiles(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	dir := t.TempDir()
	backup, err := f.app.Backup(secondActor, secondProject, dir)
	if err != nil {
		t.Fatal(err)
	}
	path := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	now := f.at.Add(defaultBackupExpiry)
	app, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	invalid := dir + "\x00invalid"
	intent := backupExpiry{Directory: invalid, Name: filepath.Base(backup.Path), MinimumAge: defaultBackupExpiry}
	if err := app.db.Update(func(tx *bolt.Tx) error { return putJSON(tx, expiryBucket, intent.key(), intent) }); err != nil {
		t.Fatal(err)
	}
	count, err := app.ExpireBackups(secondActor, secondProject, invalid)
	var native *os.PathError
	wantedPath := filepath.Join(invalid, intent.Name)
	if runtime.GOOS == "windows" {
		if count != 0 || !errors.Is(err, syscall.EINVAL) || errors.As(err, &native) {
			t.Errorf("expiry did not reject the invalid Windows absolute path before lookup: %d, %v", count, err)
		}
	} else if count != 0 || !errors.As(err, &native) || !strings.EqualFold(native.Op, "lstat") || native.Path != wantedPath {
		t.Errorf("expiry did not report the exact first-file native lookup: %d, %v", count, err)
	}
	if _, err := os.Stat(backup.Path + ".sha256"); err != nil {
		t.Errorf("failed native lookup affected an independently owned pair: %v", err)
	}
	if raw, err := os.ReadFile(backup.Path); err != nil || digest(raw) != backup.SHA256 {
		t.Errorf("failed native lookup changed the held backup bytes: %v", err)
	}
	if raw, err := app.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(raw, f.source) {
		t.Errorf("failed native lookup changed independent evidence: %v", err)
	}
	if report, html, err := app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(html, f.html) {
		t.Errorf("failed native lookup changed the prior frozen artifact: %v", err)
	}
}
