package board

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"unicode/utf16"
)

const (
	windowsInvalidPathName   = syscall.Errno(123) // ERROR_INVALID_NAME.
	windowsBackupNameTooLong = syscall.Errno(206) // ERROR_FILENAME_EXCED_RANGE.
)

func isWindowsLengthRefusal(err error) bool {
	return errors.Is(err, syscall.ERROR_PATH_NOT_FOUND) ||
		errors.Is(err, windowsBackupNameTooLong) || errors.Is(err, windowsInvalidPathName)
}

func TestFollowupBackupRefusesWindowsNativePublicationPaths(t *testing.T) {
	t.Run("final_rename", func(t *testing.T) {
		parentUnits := windowsStageParentUnits(t)
		directory, err := exerciseWindowsBackupPathFault(t, parentUnits)
		var rename *os.LinkError
		if !errors.As(err, &rename) || rename.Op != "rename" ||
			!isWindowsLengthRefusal(err) {
			t.Fatal("backup did not preserve the native final rename error")
		}
		if filepath.Dir(rename.Old) != directory || filepath.Dir(rename.New) != directory ||
			!strings.HasPrefix(filepath.Base(rename.Old), "reb-stage-") || !strings.HasSuffix(rename.Old, ".db") ||
			!strings.HasPrefix(filepath.Base(rename.New), "reb-backup-") || !strings.HasSuffix(rename.New, ".db") ||
			windowsBackupPathUnits(rename.Old) > parentUnits+24 || windowsBackupPathUnits(rename.New) != parentUnits+51 {
			t.Fatal("native rename error did not identify the owned staged and final paths")
		}
		// Repeat the native operation with a new owned source. The path spelling
		// alone must reproduce the same refusal without the application.
		if err := os.WriteFile(rename.Old, []byte("control"), 0600); err != nil {
			t.Fatal("could not recreate the short stage path")
		}
		controlErr := os.Rename(rename.Old, rename.New)
		if !errors.Is(controlErr, rename.Err) {
			t.Fatal("independent rename did not reproduce the native publication error")
		}
		if err := os.Remove(rename.Old); err != nil {
			t.Fatal("could not remove the native rename control")
		}
	})
}

func exerciseWindowsBackupPathFault(t *testing.T, directoryUnits int) (string, error) {
	t.Helper()
	fixture := newIntakeCustodyFixture(t)
	prior, err := fixture.app.Backup(secondActor, secondProject, t.TempDir())
	if err != nil {
		t.Fatalf("prior short-path backup failed: %v", err)
	}
	priorDatabase, priorChecksum := readWindowsBackupPair(t, prior)
	directory := newWindowsBackupPath(t, directoryUnits)
	control, err := os.CreateTemp(directory, "reb-stage-*.db")
	if err != nil {
		t.Fatalf("native CreateTemp failed before publication, error type %T", err)
	}
	controlUnits := windowsBackupPathUnits(control.Name())
	if err := errors.Join(control.Close(), os.Remove(control.Name())); err != nil {
		t.Fatalf("cannot clean the temporary snapshot control, error type %T", err)
	}
	t.Logf("owned parent %d UTF-16 units admitted temporary snapshot at %d units", directoryUnits, controlUnits)
	result, publicationErr := fixture.app.Backup(secondActor, secondProject, directory)
	var native syscall.Errno
	errors.As(publicationErr, &native)
	var rename *os.LinkError
	var open *os.PathError
	switch {
	case errors.As(publicationErr, &rename):
		t.Logf("native %s old=%d new=%d errno=%d", rename.Op, windowsBackupPathUnits(rename.Old), windowsBackupPathUnits(rename.New), native)
	case errors.As(publicationErr, &open):
		t.Logf("native %s path=%d filename=%s errno=%d", open.Op, windowsBackupPathUnits(open.Path), filepath.Base(open.Path), native)
	}
	if result != (Backup{}) || publicationErr == nil {
		t.Error("failed publication did not return an empty backup record and native error")
	}
	if leaves, err := os.ReadDir(directory); err != nil || len(leaves) != 0 {
		t.Errorf("failed publication left the owned directory nonempty or unreadable: entries=%d error type=%T", len(leaves), err)
	}
	reopened := fixture.checkReopened(t, false)
	health, err := reopened.Health(secondActor, secondProject)
	database, checksum := readWindowsBackupPair(t, prior)
	if err != nil || !health.LastBackup.Equal(prior.CreatedAt) || !bytes.Equal(database, priorDatabase) || !bytes.Equal(checksum, priorChecksum) {
		t.Fatalf("prior normal backup pair or health timestamp changed after reopen: %v", err)
	}
	controlBackup, err := reopened.Backup(secondActor, secondProject, t.TempDir())
	if err != nil {
		t.Fatalf("short-path backup after reopen failed: %v", err)
	}
	readWindowsBackupPair(t, controlBackup)
	t.Logf("checked prior backup pair, receipt, original CSV and frozen HTML after reopen and verified a new short-path backup")
	return directory, publicationErr
}

func readWindowsBackupPair(t *testing.T, backup Backup) ([]byte, []byte) {
	t.Helper()
	database, err := os.ReadFile(backup.Path)
	if err != nil {
		t.Fatal(err)
	}
	checksum, err := os.ReadFile(backup.Path + ".sha256")
	if err != nil || digest(database) != backup.SHA256 || string(checksum) != backup.SHA256+"\n" {
		t.Fatalf("normal backup pair did not match its published SHA-256: %v", err)
	}
	return database, checksum
}

func newWindowsBackupPath(t *testing.T, directoryUnits int) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	volume := filepath.VolumeName(root)
	if err != nil || !filepath.IsAbs(root) || len(volume) != 2 || volume[1] != ':' {
		t.Fatalf("cannot resolve the owned local-drive path: %q: %v", root, err)
	}
	directory := `\\?\` + root
	var owned []string
	t.Cleanup(func() {
		if len(owned) == 0 {
			return
		}
		deepest := owned[len(owned)-1]
		entries, err := os.ReadDir(deepest)
		if err != nil {
			t.Errorf("cannot inspect owned path before cleanup, error type %T", err)
			return
		}
		for _, entry := range entries {
			if err := os.Remove(filepath.Join(deepest, entry.Name())); err != nil {
				t.Errorf("cannot clean owned publication leaf %q, error type %T", entry.Name(), err)
				return
			}
		}
		for i := len(owned) - 1; i >= 0; i-- {
			if err := os.Remove(owned[i]); err != nil {
				t.Errorf("cannot remove owned path component %d, error type %T", i+1, err)
				return
			}
		}
		t.Logf("cleaned %d owned directory components", len(owned))
	})
	for units := windowsBackupPathUnits(directory); units < directoryUnits; units = windowsBackupPathUnits(directory) {
		remaining := directoryUnits - units - 1
		width := min(remaining, 200)
		if remaining == 201 {
			width--
		}
		if width < 1 || len(owned) >= 200 {
			t.Fatal("cannot build the bounded native path fixture")
		}
		directory = filepath.Join(directory, strings.Repeat("x", width))
		if err := os.Mkdir(directory, 0700); err != nil {
			var native syscall.Errno
			errors.As(err, &native)
			t.Fatalf("native owned directory creation stopped before publication at %d units, errno=%d", windowsBackupPathUnits(directory), native)
		}
		owned = append(owned, directory)
	}
	if windowsBackupPathUnits(directory) != directoryUnits {
		t.Fatal("owned native directory has an unexpected path length")
	}
	t.Logf("owned path root %s, components %d, UTF-16 units %d", root, len(owned), directoryUnits)
	return directory
}

// Measure the native boundary on the same volume used by the application test.
func windowsStageParentUnits(t *testing.T) int {
	t.Helper()
	const baseUnits = 32600
	base := newWindowsBackupPath(t, baseUnits)
	limit := func(directory bool) int {
		const maxWidth = 200
		width := sort.Search(maxWidth, func(index int) bool {
			path := filepath.Join(base, strings.Repeat("p", index+1))
			return !windowsControlFits(t, path, directory)
		})
		if width == 0 || width == maxWidth {
			t.Fatal("could not bracket the native path limit on the test volume")
		}
		return baseUnits + 1 + width
	}
	dirLimit, fileLimit := limit(true), limit(false)
	// Each randomized stage name can add up to 24 units to its parent.
	parentUnits := min(dirLimit, fileLimit) - 24
	if parentUnits+24+len(`\store.db`) <= fileLimit {
		t.Fatalf("cannot isolate a stage-file refusal: directory limit=%d file limit=%d", dirLimit, fileLimit)
	}
	t.Logf("native path controls: directory=%d file=%d parent=%d UTF-16 units", dirLimit, fileLimit, parentUnits)
	return parentUnits
}

func windowsControlFits(t *testing.T, path string, directory bool) bool {
	t.Helper()
	var err error
	if directory {
		err = os.Mkdir(path, 0700)
	} else {
		var file *os.File
		file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			if closeErr := file.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		}
	}
	if err != nil {
		if !isWindowsLengthRefusal(err) {
			t.Fatalf("unexpected native control error at %d UTF-16 units: %v", windowsBackupPathUnits(path), errors.Unwrap(err))
		}
		return false
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	return true
}

func windowsBackupPathUnits(path string) int { return len(utf16.Encode([]rune(path))) }
