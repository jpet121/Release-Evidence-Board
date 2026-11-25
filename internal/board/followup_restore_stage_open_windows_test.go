package board

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFollowupRestoreCleansWindowsNativeStageOpenRefusal(t *testing.T) {
	fixture := newIntakeCustodyFixture(t)
	backup, err := fixture.app.Backup(secondActor, secondProject, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prior, priorChecksum := readWindowsBackupPair(t, backup)
	directory := newWindowsBackupPath(t, 32735)
	destination := filepath.Join(directory, "held.db")
	settings := Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return fixture.at }}
	// A short random decimal suffix can fit. Retry only a verified success
	// in the same parent, with a freshly absent destination each time.
	var result RestoreResult
	for range 4 {
		result, err = RestoreBackup(backup.Path, destination, settings)
		if err != nil {
			break
		}
		if !result.IntegrityOK {
			t.Fatal("fitting restore path did not preserve verified backup")
		}
		if err := os.Remove(destination); err != nil {
			t.Fatalf("could not clean an owned fitting restore, error type %T", err)
		}
	}
	if result != (RestoreResult{}) || err == nil {
		t.Error("stage open refusal did not return an empty restore result and native error")
	}
	if _, statusErr := os.Lstat(destination); !errors.Is(statusErr, os.ErrNotExist) {
		t.Error("restore stage refusal published a destination or left its identity unreadable")
	}
	if entries, readErr := os.ReadDir(directory); readErr != nil || len(entries) != 0 {
		t.Errorf("restore stage refusal left %d entries in the owned parent, error type %T", len(entries), readErr)
	}
	var native *os.PathError
	var code syscall.Errno
	if errors.As(err, &native) && errors.As(native.Err, &code) {
		t.Logf("native %s path=%d parent=%d filename=%s errno=%d", native.Op,
			windowsBackupPathUnits(native.Path), windowsBackupPathUnits(filepath.Dir(native.Path)), filepath.Base(native.Path), code)
	}
	if native == nil || native.Op != "open" || (code != syscall.ERROR_PATH_NOT_FOUND && code != windowsBackupNameTooLong) {
		t.Fatal("restore did not preserve the native exclusive stage-file open error")
	}
	stage := filepath.Dir(native.Path)
	if filepath.Dir(stage) != directory || !strings.HasPrefix(filepath.Base(stage), ".reb-restore-") || filepath.Base(native.Path) != "store.db" ||
		windowsBackupPathUnits(stage) > 32759 || windowsBackupPathUnits(native.Path) != windowsBackupPathUnits(stage)+9 {
		t.Fatal("native stage-file error did not identify the owned restore stage")
	}
	// Recreating the same stage establishes that directory creation fits while the
	// initial exclusive file open itself fails, before store verification.
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatalf("exact restore stage directory could not be recreated, error type %T", err)
	}
	file, openErr := os.OpenFile(native.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if file != nil {
		if err := errors.Join(file.Close(), os.Remove(native.Path)); err != nil {
			t.Errorf("could not clean the native stage file control, error type %T", err)
		}
	}
	if err := os.Remove(stage); err != nil {
		t.Errorf("could not clean the native stage directory control, error type %T", err)
	}
	if file != nil || !errors.Is(openErr, code) {
		t.Fatal("same stage directory fitted but its independent exclusive file open did not reproduce the native error")
	}
	positive := filepath.Join(t.TempDir(), "restored.db")
	if restored, err := RestoreBackup(backup.Path, positive, settings); err != nil || !restored.IntegrityOK {
		t.Fatalf("ordinary short-path restore failed: %v", err)
	}
	database, checksum := readWindowsBackupPair(t, backup)
	if !bytes.Equal(database, prior) || !bytes.Equal(checksum, priorChecksum) {
		t.Fatal("restore attempt changed the source backup pair")
	}
	fixture.checkReopened(t, false)
	t.Log("checked prior backup pair, receipt, original CSV and frozen HTML after reopen and verified a short-path restore")
}
