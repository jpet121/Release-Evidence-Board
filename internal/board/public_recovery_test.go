package board_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestOperatorRecoveryKeepsPriorStateOnRealFileConflicts(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	candidate := fixtureCandidate(t, app, vectorBaseline())
	root := t.TempDir()
	occupied := filepath.Join(root, "occupied")
	const existingData = "retained file contents"
	if err := os.WriteFile(occupied, []byte(existingData), 0600); err != nil {
		t.Fatal(err)
	}
	if backup, err := app.Backup(supervisor, project, occupied); err == nil || backup.Path != "" {
		t.Fatal("backup reported success using an existing file as its directory")
	}
	if health, err := app.Health(supervisor, project); err != nil || !health.LastBackup.IsZero() {
		t.Fatal("failed backup advanced the latest successful-backup setting")
	}
	if err := app.RecordRestoreDrill(supervisor, project, occupied); err == nil {
		t.Fatal("restore drill accepted a file without a valid database")
	}
	if health, err := app.Health(supervisor, project); err != nil || !health.LastDrill.IsZero() {
		t.Fatal("failed drill advanced the verified restore-drill timestamp")
	}
	backup, err := app.Backup(supervisor, project, filepath.Join(root, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	settings := board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return fixtureTime }}
	if result, err := board.RestoreBackup(backup.Path, occupied, settings); !errors.Is(err, os.ErrExist) || result.IntegrityOK {
		t.Fatalf("restore overwrote existing destination or misreported success: %v", err)
	}
	if data, err := os.ReadFile(occupied); err != nil || string(data) != existingData {
		t.Fatalf("restore changed the existing file: %v", err)
	}
	foreign := settings
	foreign.ProjectID = "foreign"
	abandoned := filepath.Join(root, "foreign.db")
	if result, err := board.RestoreBackup(backup.Path, abandoned, foreign); err == nil || result.IntegrityOK {
		t.Fatal("restore recreated an existing project under a foreign identity")
	}
	if _, err := os.Stat(abandoned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed restore retained untrusted target bytes: %v", err)
	}
	if state, err := app.Assess(supervisor, project, candidate.ID); err != nil || state.Status != board.Blocked || state.LedgerHighWater != 0 {
		t.Fatalf("failed external restore modified the original evidence: %v", err)
	}
}

func TestRestoreCleansCreatedFileWhenSourceOrChecksumCannotBeRead(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	backup, err := app.Backup(supervisor, project, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(backup.Path + ".sha256"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(backup.Path+".sha256", 0700); err != nil {
		t.Fatal(err)
	}
	settings := board.Settings{ProjectID: project, ProductID: "DEMO"}
	for _, source := range []string{t.TempDir(), backup.Path} {
		path := filepath.Join(t.TempDir(), "must-remain-absent.db")
		if result, err := board.RestoreBackup(source, path, settings); err == nil || result.IntegrityOK {
			t.Fatal("restore reported an unreadable source or checksum validated")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("copy failure retained an incomplete restore file: %v", err)
		}
	}
}
