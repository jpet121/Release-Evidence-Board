package board

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupTwoFaultyHealthChecksResetAfterRealRecovery(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	app, err := Open(filepath.Join(t.TempDir(), "health.db"), Settings{
		ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	}()
	// A real storage format fault creates a failed health check. Restore
	// the prior exact data before observing recovery on the same Board.
	var original []byte
	if err := app.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(metaBucket))
		if retained := bucket.Get([]byte("ledger")); retained != nil {
			original = append([]byte{}, retained...)
		}
		return bucket.Put([]byte("ledger"), []byte("synthetic invalid counter"))
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{1, 2} {
		now = now.Add(time.Minute)
		check := app.Check()
		if check.DatabaseOK || check.ConsecutiveFailures != want || !check.CheckedAt.Equal(now) {
			t.Fatalf("health fault %d not observed at the expected check: %+v", want, check)
		}
	}
	if err := app.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(metaBucket))
		if original == nil {
			return bucket.Delete([]byte("ledger"))
		}
		return bucket.Put([]byte("ledger"), original)
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	recovered := app.Check()
	if !recovered.DatabaseOK || recovered.ConsecutiveFailures != 0 || !recovered.CheckedAt.Equal(now) {
		t.Fatalf("successful read did not reset the health failures: %+v", recovered)
	}
	t.Log(`REB_ACCEPTANCE_INSTANCE {"id":"AC-025-03.P001","case_id":"AC-025-03","parameters":{"consecutiveFailuresBeforeSuccess":2},"actual":"Consecutive-failure counter resets"}`)
}

func TestFollowupLatestBackupAgeControlsHealthWithOlderBackupRetained(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	settings := Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return now }}
	app, err := Open(filepath.Join(t.TempDir(), "fixture.db"), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := app.Bootstrap("reviewer", "synthetic-backup-age"); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	older, err := app.Backup("reviewer", "pilot", directory)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	newer, err := app.Backup("reviewer", "pilot", directory)
	if err != nil || newer.Path == older.Path {
		t.Fatalf("repeat daily backup did not retain a distinct snapshot: %v", err)
	}
	restored := filepath.Join(t.TempDir(), "restore.db")
	verification, err := RestoreBackup(newer.Path, restored, settings)
	if err != nil || !verification.IntegrityOK {
		t.Fatalf("could not verify fresh daily backup: %v", err)
	}
	if err := app.RecordRestoreDrill("reviewer", "pilot", restored); err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		age     time.Duration
		overdue bool
	}{{26 * time.Hour, false}, {26*time.Hour + time.Microsecond, true}} {
		now = newer.CreatedAt.Add(sample.age)
		check := app.Check()
		health, err := app.Health("reviewer", "pilot")
		if err != nil || !health.LastBackup.Equal(newer.CreatedAt) || !health.DatabaseOK ||
			check.BackupOverdue != sample.overdue || strings.Contains(health.Alert, "more than 26 hours") != sample.overdue {
			t.Fatalf("newest backup age %s has inaccurate alert %q: %v", sample.age, health.Alert, err)
		}
	}
	if _, err := os.Stat(older.Path); err != nil {
		t.Fatalf("health check did not retain older backup: %v", err)
	}
	t.Log(`REB_ACCEPTANCE_INSTANCE {"id":"AC-025-04.P001","case_id":"AC-025-04","parameters":{"latestSuccessfulBackupAge":"26:00:00","expectedAlert":false},"actual":"No alert at equality. Emit an overdue alert after 26 hours based on the most recent success"}`)
	t.Log(`REB_ACCEPTANCE_INSTANCE {"id":"AC-025-04.P002","case_id":"AC-025-04","parameters":{"latestSuccessfulBackupAge":"26:00:00.000001","expectedAlert":true},"actual":"No alert at equality. Emit an overdue alert after 26 hours based on the most recent success"}`)
}
