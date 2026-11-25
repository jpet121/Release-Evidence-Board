package board_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupBackupExpirySelectsOnlyEligibleFilePairs(t *testing.T) {
	now := fixtureTime
	app, _ := openBoard(t, func() time.Time { return now })
	directory := t.TempDir()
	backup, err := app.Backup(supervisor, project, directory)
	if err != nil {
		t.Fatal(err)
	}
	read := func(path string) []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := read(backup.Path)
	companion := backup.Path + ".sha256"
	checksum := read(companion)
	name := filepath.Base(backup.Path)
	const prefix = "reb-backup-"
	alternatives := []string{
		name + "x",
		"x" + name[1:],
		name[:len(name)-1] + "x",
		prefix + "x" + name[len(prefix)+1:],
		name[:len(prefix)+23] + "_" + name[len(prefix)+24:],
		name[:len(prefix)+24] + "g" + name[len(prefix)+25:],
	}
	for _, other := range alternatives {
		path := filepath.Join(directory, other)
		write(path, snapshot)
		write(path+".sha256", checksum)
	}
	nonFile := filepath.Join(directory, name[:len(prefix)+24]+"000000000000.db")
	if nonFile == backup.Path {
		nonFile = filepath.Join(directory, name[:len(prefix)+24]+"111111111111.db")
	}
	if err := os.Mkdir(nonFile, 0700); err != nil {
		t.Fatal(err)
	}
	now = fixtureTime.Add(30 * 24 * time.Hour)
	if count, err := app.ExpireBackups(supervisor, project, filepath.Join(directory, "missing")); count != 0 || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing scan directory returned %d, %v", count, err)
	}
	savedCompanion := filepath.Join(t.TempDir(), "saved.sha256")
	if err := os.Rename(companion, savedCompanion); err != nil {
		t.Fatal(err)
	}
	if count, err := app.ExpireBackups(supervisor, project, directory); count != 0 || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("eligible snapshot without its companion returned %d, %v", count, err)
	}
	if !bytes.Equal(read(backup.Path), snapshot) || !bytes.Equal(read(savedCompanion), checksum) {
		t.Fatal("expiry without its companion changed the original pair")
	}
	events, err := app.Audit(supervisor, project)
	if err != nil || slices.ContainsFunc(events, func(event board.AuditEvent) bool { return event.Action == "backup.expire_intent" }) {
		t.Fatalf("failed scan recorded an expiry intent: %v", err)
	}
	if err := os.Rename(savedCompanion, companion); err != nil {
		t.Fatal(err)
	}
	if count, err := app.ExpireBackups(supervisor, project, directory); count != 1 || err != nil {
		t.Fatalf("expiry of the complete pair returned %d, %v", count, err)
	}
	for _, path := range []string{backup.Path, companion} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("eligible pair remained at %q: %v", filepath.Base(path), err)
		}
	}
	for _, other := range alternatives {
		path := filepath.Join(directory, other)
		if !bytes.Equal(read(path), snapshot) || !bytes.Equal(read(path+".sha256"), checksum) {
			t.Fatalf("expiry changed the unrelated pair %q", other)
		}
	}
	if info, err := os.Stat(nonFile); err != nil || !info.IsDir() {
		t.Fatalf("expiry changed matching directory entry: %v", err)
	}
}
