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

func TestFollowupRestoreCleansNativeStageOpenRefusal(t *testing.T) {
	const nativePathLimit = 4096
	// One slash, the 13-byte stage prefix and at most 10 random decimal
	// digits leave the stage directory at 4095 bytes. Adding /store.db
	// exceeds PATH_MAX except for the rare one-digit suffix, retried below.
	const restoreParentLength = nativePathLimit - 25
	f := newIntakeCustodyFixture(t)
	backup, err := f.app.Backup(secondActor, secondProject, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(backup.Path)
	if err != nil {
		t.Fatal(err)
	}
	sidecar, err := os.ReadFile(backup.Path + ".sha256")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if len(dir)+2 > restoreParentLength {
		t.Fatal("owned temp root too long for stage boundary")
	}
	for len(dir) < restoreParentLength {
		remaining := restoreParentLength - len(dir) - 1
		width := min(remaining, 200)
		if remaining == 201 {
			width--
		}
		dir = filepath.Join(dir, strings.Repeat("s", width))
	}
	if len(dir) != restoreParentLength {
		t.Fatal("unexpected native fixture path length")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("owned parent setup: %v", err)
	}
	settings := Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return f.at }}
	qualified := false
	for trial := 0; trial < 4; trial++ {
		destination := filepath.Join(dir, "held.db")
		result, err := RestoreBackup(backup.Path, destination, settings)
		if err == nil {
			if !result.IntegrityOK {
				t.Fatal("rare fitting path did not preserve verified backup")
			}
			if err := os.Remove(destination); err != nil {
				t.Fatal(err)
			}
			continue
		}
		var native *os.PathError
		if !errors.As(err, &native) || !errors.Is(native.Err, syscall.ENAMETOOLONG) || native.Op != "open" || filepath.Base(native.Path) != "store.db" || filepath.Dir(filepath.Dir(native.Path)) != dir || !strings.HasPrefix(filepath.Base(filepath.Dir(native.Path)), ".reb-restore-") || len(filepath.Dir(native.Path)) >= nativePathLimit || len(native.Path) < nativePathLimit {
			t.Fatalf("restore missed exact exclusive staged-file open boundary: %v", err)
		}
		if result.IntegrityOK {
			t.Fatal("failed stage open reported verified restoration")
		}
		if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refused restore published a destination: %v", err)
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("stage open refusal leaked namespace: %d entries, %v", len(entries), err)
		}
		t.Logf("native %s refused %d-byte stage file after creating %d-byte stage directory", native.Op, len(native.Path), len(filepath.Dir(native.Path)))
		qualified = true
		break
	}
	if !qualified {
		t.Fatal("four bounded native trials did not qualify stage-file refusal")
	}
	if got, err := os.ReadFile(backup.Path); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("source backup changed: %v", err)
	}
	if got, err := os.ReadFile(backup.Path + ".sha256"); err != nil || !bytes.Equal(got, sidecar) {
		t.Fatalf("checksum changed: %v", err)
	}
	positive := filepath.Join(t.TempDir(), "restored.db")
	if result, err := RestoreBackup(backup.Path, positive, settings); err != nil || !result.IntegrityOK {
		t.Fatalf("ordinary path failed valid restore: %v", err)
	}
	f.checkReopened(t, false)
}
