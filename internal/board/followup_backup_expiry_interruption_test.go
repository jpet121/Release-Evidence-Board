package board_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

const backupExpiryDir = "REB_EXPIRY_JOURNAL_FIXTURE_DIR"
const backupExpiryTrial = "REB_EXPIRY_JOURNAL_FIXTURE_TRIAL"

var backupExpiryInterruptions = []interruptionTrial{
	{"expiry-intent-durable", "purge journal", "expiry-intent-committed-before-file-unlink", "backup.expire", 3, 83},
	{"expiry-files-removed", "purge journal", "expiry-files-unlinked-before-journal-finalization", "backup.expire", 4, 84},
}

func TestFollowupBackupExpiryJournalResumesAfterChildExit(t *testing.T) {
	for _, trial := range backupExpiryInterruptions {
		t.Run(trial.ID, func(t *testing.T) {
			dir := t.TempDir()
			fixture := createInterruptionFixture(t, dir)
			backups := filepath.Join(dir, "backups")
			now := fixtureTime.Add(30*24*time.Hour + time.Hour)
			settings := componentClock(now)
			store := filepath.Join(dir, "original.db")
			app, err := board.Open(store, settings)
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := app.Backup(supervisor, project, backups)
			componentClose(t, app)
			if err != nil {
				t.Fatal(err)
			}
			protected := map[string][]byte{}
			for _, path := range []string{fresh.Path, fresh.Path + ".sha256"} {
				protected[path] = readInterruptionBytes(t, path)
			}
			other := filepath.Join(dir, "other")
			if err := os.Mkdir(other, 0700); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{fixture.Backup.Path, fixture.Backup.Path + ".sha256"} {
				copy := filepath.Join(other, filepath.Base(path))
				protected[copy] = readInterruptionBytes(t, path)
				if err := os.WriteFile(copy, protected[copy], 0600); err != nil {
					t.Fatal(err)
				}
			}
			note := filepath.Join(backups, "unrelated-notes.txt")
			protected[note] = []byte("untouched content in the operator directory\n")
			if err := os.WriteFile(note, protected[note], 0600); err != nil {
				t.Fatal(err)
			}
			expiringDB := readInterruptionBytes(t, fixture.Backup.Path)
			expiringSidecar := readInterruptionBytes(t, fixture.Backup.Path+".sha256")
			ctx, cancel := crashChildContext(t)
			defer cancel()
			log, err := runCrashTestChild(ctx, "TestFollowupBackupExpiryJournalChild", backupExpiryDir+"="+dir, backupExpiryTrial+"="+trial.ID)
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != trial.Exit || !bytes.Contains(log, []byte("REB_NATIVE_PHASE "+trial.Phase+"\n")) {
				t.Fatalf("owned backup expiry child missed phase or exit: %v; %s", err, log)
			}
			if trial.ClockCall == 3 {
				if !bytes.Equal(readInterruptionBytes(t, fixture.Backup.Path), expiringDB) ||
					!bytes.Equal(readInterruptionBytes(t, fixture.Backup.Path+".sha256"), expiringSidecar) {
					t.Fatal("durable intent changed either file before unlink")
				}
			} else {
				assertExpiryPairAbsent(t, fixture.Backup.Path)
			}
			app, err = board.Open(store, settings)
			if err != nil {
				t.Fatal(err)
			}
			defer componentClose(t, app)
			name := filepath.Base(fixture.Backup.Path)
			assertBackupExpiryAudit(t, app, name, 1, 0)
			if done, err := app.ExpireBackups(supervisor, project, backups); err != nil || done != 1 {
				t.Fatalf("reopened store did not resume exactly one durable intent: %d, %v", done, err)
			}
			if done, err := app.ExpireBackups(supervisor, project, backups); err != nil || done != 0 {
				t.Fatalf("reconciled expiry was not idempotent: %d, %v", done, err)
			}
			assertExpiryPairAbsent(t, fixture.Backup.Path)
			assertBackupExpiryAudit(t, app, name, 1, 1)
			for path, before := range protected {
				if !bytes.Equal(readInterruptionBytes(t, path), before) {
					t.Fatal("resumed expiry changed a non-target file")
				}
			}
			assertComponentImport(t, app, fixture.OldCSV, 1, fixture.OldRun)
			assertComponentImport(t, app, fixture.KeptCSV, 2, fixture.KeptRun)
			assertComponentHTML(t, app, fixture.OldReport, fixture.OldHTML)
			assertComponentHTML(t, app, fixture.KeptReport, fixture.KeptHTML)
			t.Logf("REB_COMPONENT_INTERRUPTION %s", interruptionObservation(trial, fixture, 1))
		})
	}
}

func TestFollowupBackupExpiryJournalChild(t *testing.T) {
	dir := os.Getenv(backupExpiryDir)
	if dir == "" {
		return
	}
	i := slices.IndexFunc(backupExpiryInterruptions, func(trial interruptionTrial) bool { return trial.ID == os.Getenv(backupExpiryTrial) })
	if i < 0 {
		t.Fatal("unknown backup expiry interruption phase")
	}
	trial := backupExpiryInterruptions[i]
	now := fixtureTime.Add(30*24*time.Hour + time.Hour)
	armed, clockCalls := false, 0
	settings := componentClock(now)
	settings.Clock = func() time.Time {
		if armed {
			clockCalls++
			if clockCalls == trial.ClockCall {
				fmt.Println("REB_NATIVE_PHASE " + trial.Phase)
				os.Exit(trial.Exit)
			}
		}
		return now
	}
	app, err := board.Open(filepath.Join(dir, "original.db"), settings)
	if err != nil {
		t.Fatal(err)
	}
	armed = true
	done, err := app.ExpireBackups(supervisor, project, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("backup expiry unexpectedly returned after %d items before the controlled native exit", done)
}

func readInterruptionBytes(t *testing.T, file string) []byte {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertExpiryPairAbsent(t *testing.T, path string) {
	t.Helper()
	for _, file := range []string{path, path + ".sha256"} {
		if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected exact expired backup pair to be absent: %v", err)
		}
	}
}

func assertBackupExpiryAudit(t *testing.T, app *board.Board, name string, wantIntents, wantFinishes int) {
	t.Helper()
	intents, finishes := 0, 0
	for _, event := range intakeAudit(t, app) {
		if event.Outcome != "success" || event.Actor != supervisor || !slices.Equal(event.References, []string{name}) {
			continue
		}
		switch event.Action {
		case "backup.expire_intent":
			intents++
		case "backup.expire":
			finishes++
		}
	}
	if intents != wantIntents || finishes != wantFinishes {
		t.Fatalf("wrong durable expiry audit counts: intent=%d expiry=%d, expected %d and %d", intents, finishes, wantIntents, wantFinishes)
	}
}
