package board_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupOlderClockBackupCannotRewindLastVerifiedBackup(t *testing.T) {
	now := fixtureTime
	app, _ := openBoard(t, func() time.Time { return now })
	dir := t.TempDir()
	now = now.Add(2 * time.Hour)
	newer, err := app.Backup(supervisor, project, dir)
	if err != nil || !newer.CreatedAt.Equal(now) {
		t.Fatalf("could not establish the first checked backup: %v", err)
	}
	now = now.Add(-time.Hour)
	older, err := app.Backup(supervisor, project, dir)
	if err != nil || !older.CreatedAt.Equal(now) || older.Path == newer.Path {
		t.Fatalf("independent backup with the earlier clock was unavailable: %v", err)
	}
	if health, err := app.Health(supervisor, project); err != nil || !health.LastBackup.Equal(newer.CreatedAt) {
		t.Errorf("earlier backup moved the most recent verified time: %s, %v", health.LastBackup, err)
	}
	for _, backup := range []board.Backup{newer, older} {
		raw, err := os.ReadFile(backup.Path)
		if err != nil || len(raw) == 0 || fmt.Sprintf("%x", sha256.Sum256(raw)) != backup.SHA256 {
			t.Errorf("clock ordering changed a checked backup file: %v", err)
		}
		companion, err := os.ReadFile(backup.Path + ".sha256")
		if err != nil || string(companion) != backup.SHA256+"\n" {
			t.Errorf("clock ordering changed a checked backup companion: %v", err)
		}
	}
}
