package board

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupPendingExpiryChecksCustodyAndJournalAgainBeforeUnlink(t *testing.T) {
	for _, fault := range []string{"new_hold", "missing_saved_intent"} {
		t.Run(fault, func(t *testing.T) {
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
			var armed atomic.Bool
			var pauseWriter func()
			app, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time {
				if armed.CompareAndSwap(true, false) {
					pauseWriter()
				}
				return f.at.Add(30 * 24 * time.Hour)
			}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := app.Close(); err != nil {
					t.Error(err)
				}
			})
			realLstat := app.expiryLstat
			app.expiryLstat = func(path string) (os.FileInfo, error) {
				if path == backup.Path {
					return nil, &os.PathError{Op: "lstat", Path: path, Err: os.ErrPermission}
				}
				return realLstat(path)
			}
			if n, err := app.ExpireBackups(secondActor, secondProject, directory); n != 0 || !errors.Is(err, os.ErrPermission) {
				t.Fatalf("fixture could not create an outstanding durable intent ahead of file removal: %d %v", n, err)
			}
			app.expiryLstat = realLstat
			var savedKey, savedIntent []byte
			write := func(pause func()) error {
				if fault == "new_hold" {
					pauseWriter = pause
					armed.Store(true)
					return app.SuspendAssessments(secondActor, secondProject)
				}
				return app.db.Update(func(tx *bolt.Tx) error {
					journal := tx.Bucket([]byte(expiryBucket))
					key, value := journal.Cursor().First()
					if key == nil || value == nil {
						return errors.New("fixture lacks its outstanding parsed expiry intent")
					}
					savedKey, savedIntent = bytes.Clone(key), bytes.Clone(value)
					if err := journal.Delete(key); err != nil {
						return err
					}
					pause()
					return nil
				})
			}
			type result struct {
				count int
				err   error
			}
			observed := afterOldStoreRead(t, app, write, func() result {
				count, err := app.ExpireBackups(secondActor, secondProject, directory)
				return result{count, err}
			})
			if observed.count != 0 || !errors.Is(observed.err, ErrIntegrity) {
				t.Fatalf("outstanding expiry ignored the state committed after its old read: %d %v", observed.count, observed.err)
			}
			if fault == "missing_saved_intent" && !strings.Contains(observed.err.Error(), "pending backup expiry no longer matches its recorded intent") {
				t.Fatalf("outstanding expiry did not report the exact lost journal: %v", observed.err)
			}
			if raw, err := os.ReadFile(backup.Path); err != nil || digest(raw) != backup.SHA256 {
				t.Fatalf("refused pending expiry changed the original backup before verification: %v", err)
			}
			if raw, err := os.ReadFile(backup.Path + ".sha256"); err != nil || string(raw) != backup.SHA256+"\n" {
				t.Fatalf("refused pending expiry changed the original checksum: %v", err)
			}
			if fault == "missing_saved_intent" {
				if err := app.db.Update(func(tx *bolt.Tx) error {
					return tx.Bucket([]byte(expiryBucket)).Put(savedKey, savedIntent)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := app.Revalidate(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			if n, err := app.ExpireBackups(secondActor, secondProject, directory); n != 1 || err != nil {
				t.Fatalf("verified journal retry did not complete its original intent: %d %v", n, err)
			}
			if after, err := os.ReadDir(directory); err != nil || len(after) != 0 {
				t.Fatalf("verified retry did not remove the exact snapshot pair: %v %v", after, err)
			}
			if raw, err := app.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(raw, f.source) {
				t.Fatalf("pending expiry changed the retained original: %v", err)
			}
			if report, html, err := app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(html, f.html) {
				t.Fatalf("pending expiry changed the historical report: %v", err)
			}
		})
	}
}
