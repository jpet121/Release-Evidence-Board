package board_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

const (
	expiryWorkingDirectoryStore = "REB_TEST_EXPIRY_CWD_STORE"
	expiryWorkingDirectoryPhase = "REB_NATIVE_PHASE expiry-relative-path-getwd-ENOENT"
)

func TestFollowupExpiryRejectsUnlinkedWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	fixture := createInterruptionFixture(t, dir)
	store := filepath.Join(dir, "original.db")
	path := fixture.Backup.Path
	beforeDB := readInterruptionBytes(t, path)
	beforeSidecar := readInterruptionBytes(t, path+".sha256")
	settings := componentClock(fixtureTime.Add(30*24*time.Hour + time.Hour))
	app, err := board.Open(store, settings)
	if err != nil {
		t.Fatal(err)
	}
	beforeAudit := intakeAudit(t, app)
	componentClose(t, app)

	ctx, cancel := crashChildContext(t)
	defer cancel()
	output, err := runCrashTestChild(ctx, "TestFollowupExpiryWorkingDirectoryChild", expiryWorkingDirectoryStore+"="+store)
	if err != nil || !bytes.Contains(output, []byte(expiryWorkingDirectoryPhase+"\n")) {
		t.Fatalf("owned expiry child missed the qualified getwd failure: %v\n%s", err, output)
	}
	if !bytes.Equal(readInterruptionBytes(t, path), beforeDB) ||
		!bytes.Equal(readInterruptionBytes(t, path+".sha256"), beforeSidecar) {
		t.Fatal("relative path failure changed the existing backup pair")
	}

	app, err = board.Open(store, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer componentClose(t, app)
	if !reflect.DeepEqual(intakeAudit(t, app), beforeAudit) {
		t.Fatal("absolute-path resolution failure changed the durable expiry audit")
	}
	assertComponentImport(t, app, fixture.KeptCSV, 2, fixture.KeptRun)
	assertComponentHTML(t, app, fixture.KeptReport, fixture.KeptHTML)

	if count, err := app.ExpireBackups(supervisor, project, filepath.Dir(path)); err != nil || count != 1 {
		t.Fatalf("absolute path did not expire exactly the original eligible pair: %d, %v", count, err)
	}
	assertExpiryPairAbsent(t, path)
	assertBackupExpiryAudit(t, app, filepath.Base(path), 1, 1)
	t.Logf("child joined at %s; backup, audit, receipt and frozen report preserved before the absolute-path control", expiryWorkingDirectoryPhase)
}

func TestFollowupExpiryWorkingDirectoryChild(t *testing.T) {
	store := os.Getenv(expiryWorkingDirectoryStore)
	if store == "" {
		return
	}
	if !filepath.IsAbs(store) {
		t.Fatal("expiry fixture store must be outside the disposable working directory")
	}
	app, err := board.Open(store, componentClock(fixtureTime.Add(30*24*time.Hour+time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	defer componentClose(t, app)

	cwd := filepath.Join(t.TempDir(), "owned-working-directory")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	t.Setenv("PWD", "")
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	if dir, err := os.Getwd(); dir != "" || !isExpiryGetwdFailure(err) {
		t.Fatalf("native getwd did not refuse the unlinked child directory: %q, %v", dir, err)
	}
	if dir, err := filepath.Abs("backups"); dir != "" || !isExpiryGetwdFailure(err) {
		t.Fatalf("native filepath.Abs did not propagate the qualified getwd failure: %q, %v", dir, err)
	}
	count, err := app.ExpireBackups(supervisor, project, "backups")
	if count != 0 || !isExpiryGetwdFailure(err) {
		t.Fatalf("backup expiry did not return the qualified native getwd error: count=%d, %v", count, err)
	}
	fmt.Println(expiryWorkingDirectoryPhase)
}

func isExpiryGetwdFailure(err error) bool {
	var native *os.SyscallError
	return errors.As(err, &native) && native.Syscall == "getwd" && errors.Is(native.Err, syscall.ENOENT)
}
