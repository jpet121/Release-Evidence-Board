package board_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestTicket85ColdHealthKeepsBackupSnapshotConsistent(t *testing.T) {
	oldTime := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	now := oldTime
	var app *board.Board
	var fresh board.Backup
	var backupError error
	calls := 0
	intercept := false
	backups := t.TempDir()
	clock := func() time.Time {
		if intercept {
			calls++
			if calls == 2 {
				// The cold probe samples time after its authorized health read
				// has closed. Publish a real backup at that boundary.
				intercept = false
				fresh, backupError = app.Backup("reviewer", "pilot", backups)
			}
		}
		return now
	}
	var err error
	app, err = board.Open(filepath.Join(t.TempDir(), "health-snapshot.db"), board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := app.Bootstrap("reviewer", "synthetic-health-snapshot-password"); err != nil {
		t.Fatal(err)
	}
	prior, err := app.Backup("reviewer", "pilot", backups)
	if err != nil {
		t.Fatal(err)
	}
	now = oldTime.Add(27 * time.Hour)
	intercept = true
	health, err := app.Health("reviewer", "pilot")
	if err != nil || backupError != nil || fresh.Path == "" {
		t.Fatalf("real backup at cold-health boundary failed: %v %v", err, backupError)
	}
	if !health.DatabaseOK {
		t.Fatalf("healthy probe failed: %+v", health)
	}
	switch {
	case health.LastBackup.Equal(prior.CreatedAt):
		if !health.BackupDue || !health.BackupOverdue || !strings.Contains(health.Alert, "26 hours") {
			t.Fatalf("old backup timestamp was mixed with later backup flags: %+v", health)
		}
	case health.LastBackup.Equal(fresh.CreatedAt):
		if health.BackupDue || health.BackupOverdue || strings.Contains(health.Alert, "26 hours") {
			t.Fatalf("fresh backup timestamp was mixed with old backup flags: %+v", health)
		}
	default:
		t.Fatalf("health returned neither real committed backup snapshot: %+v", health)
	}
}
