package board

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestFollowupBackupExpiryKeepsPairWhenFirstFileStatusIsUnavailable(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	directory := t.TempDir()
	backup, err := f.app.Backup(secondActor, secondProject, directory)
	if err != nil {
		t.Fatal(err)
	}
	pair := []string{backup.Path, backup.Path + ".sha256"}
	read := func(path string) []byte {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	original := [][]byte{read(pair[0]), read(pair[1])}
	path := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	settings := Settings{
		ProjectID: secondProject, ProductID: "DEMO",
		Clock: func() time.Time { return f.at.Add(30 * 24 * time.Hour) },
	}
	open := func() *Board {
		t.Helper()
		app, err := Open(path, settings)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := app.Close(); err != nil {
				t.Error(err)
			}
		})
		return app
	}
	assertAudit := func(app *Board, wantFinished int) {
		t.Helper()
		events, err := app.Audit(secondActor, secondProject)
		if err != nil {
			t.Fatal(err)
		}
		intents, finished := 0, 0
		for _, event := range events {
			if event.Outcome != "success" || !slices.Equal(event.References, []string{filepath.Base(backup.Path)}) {
				continue
			}
			switch event.Action {
			case "backup.expire_intent":
				intents++
			case "backup.expire":
				finished++
			}
		}
		if intents != 1 || finished != wantFinished {
			t.Fatalf("expiry changed its durable progress: intents=%d finished=%d, expected 1 and %d", intents, finished, wantFinished)
		}
	}
	app := open()
	realLstat := app.expiryLstat
	fault := &os.PathError{Op: "lstat", Path: pair[0], Err: os.ErrPermission}
	var observed string
	app.expiryLstat = func(path string) (os.FileInfo, error) {
		if path == pair[0] && observed == "" {
			observed = path
			return nil, fault
		}
		return realLstat(path)
	}
	done, err := app.ExpireBackups(secondActor, secondProject, directory)
	if observed != pair[0] || done != 0 || !errors.Is(err, fault) || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expiry did not surface the first-file status failure: path=%q completed=%d err=%v", observed, done, err)
	}
	for i, path := range pair {
		if !bytes.Equal(read(path), original[i]) {
			t.Fatalf("status failure changed the backup pair at %q", path)
		}
	}
	assertAudit(app, 0)
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := open()
	assertAudit(reopened, 0)
	if done, err := reopened.ExpireBackups(secondActor, secondProject, directory); done != 1 || err != nil {
		t.Fatalf("healthy retry did not finish the durable expiry: completed=%d err=%v", done, err)
	}
	for _, path := range pair {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expiry did not remove %q: %v", path, err)
		}
	}
	assertAudit(reopened, 1)
	if done, err := reopened.ExpireBackups(secondActor, secondProject, directory); done != 0 || err != nil {
		t.Fatalf("finished expiry was not idempotent: completed=%d err=%v", done, err)
	}
	if csv, err := reopened.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(csv, f.source) {
		t.Fatalf("expiry changed the original retained CSV: %v", err)
	}
	if report, html, err := reopened.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(html, f.html) {
		t.Fatalf("expiry changed the prior frozen report: %v", err)
	}
}
