package board_test

import (
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"release-evidence-board/internal/board"
)

func TestTicket80HealthCheckRejectsMalformedBackupTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health-metadata.db")
	settings := board.Settings{ProjectID: "pilot", ProductID: "DEMO"}
	app, err := board.Open(path, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if app != nil {
			if err := app.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	if err := app.Bootstrap("reviewer", "synthetic-health-metadata-password"); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	app = nil
	// Corrupt the closed real storage adapter, not an in-memory collaborator.
	// Recovery must expose unhealthy storage rather than inventing a timestamp.
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("meta")).Put([]byte("last_backup"), []byte("not-a-timestamp"))
	})
	closeError := db.Close()
	if err != nil || closeError != nil {
		t.Fatalf("native metadata fault setup: %v %v", err, closeError)
	}
	app, err = board.Open(path, settings)
	if err != nil {
		t.Fatal(err)
	}
	if check := app.Check(); check.DatabaseOK || check.ConsecutiveFailures != 1 {
		t.Fatalf("malformed recovery timestamp was accepted as healthy storage: %+v", check)
	}
}
