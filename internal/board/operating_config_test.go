package board_test

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"release-evidence-board/internal/board"
)

func TestOperatingConfigurationControlsCustodyAndPreviews(t *testing.T) {
	now := fixtureTime
	app, path := openBoard(t, func() time.Time { return now })
	candidate := fixtureCandidate(t, app, vectorBaseline())
	stamp := board.FormatUTC(now)
	bind := func(run, result string) ([]byte, board.Preview) {
		t.Helper()
		raw := csvBytes(t, resultRow(candidate, "T1", "1", run, result, stamp, stamp, "PASS"))
		preview, err := app.PreviewImport(supervisor, project, candidate.ID, raw)
		if err != nil || preview.Token == "" {
			t.Fatalf("prepare real import %s: token=%q err=%v", run, preview.Token, err)
		}
		return raw, preview
	}
	raw, original := bind("INITIAL-RUN", "INITIAL-RESULT")
	receipt, err := app.CommitImport(supervisor, project, original.Token, raw)
	if err != nil {
		t.Fatal(err)
	}
	report, err := app.Freeze(supervisor, project, candidate.ID)
	if err != nil || report.Status != board.Sufficient {
		t.Fatalf("freeze retained report: %+v, %v", report, err)
	}
	_, originalHTML, err := app.ReportHTML(supervisor, project, report.ID)
	if err != nil {
		t.Fatal(err)
	}
	pendingRaw, pending := bind("PENDING-RUN", "PENDING-RESULT")
	scope := []string{candidate.ID}
	stale, err := app.PreviewPurge(supervisor, project, scope)
	if err != nil || !slices.Equal(stale.Ineligible, scope) {
		t.Fatalf("initial purge plan did not hold young candidate: %+v %v", stale, err)
	}
	oldEvents, err := app.Audit(supervisor, project)
	if err != nil || len(oldEvents) == 0 {
		t.Fatalf("missing attributed fixture events: %v", err)
	}

	now = now.Add(time.Minute)
	if err := app.SetOperatingDuration(supervisor, project, "evidence-retention", 48*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := app.CommitImport(supervisor, project, pending.Token, pendingRaw); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("uncommitted import preview survived config revision: %v", err)
	}
	retry, err := app.CommitImport(supervisor, project, original.Token, raw)
	if err != nil || !reflect.DeepEqual(retry, receipt) {
		t.Fatalf("revision invalidated exact committed import retry: %+v, %v", retry, err)
	}
	if _, err := app.Purge(supervisor, project, scope, stale.Confirmation); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("purge accepted pre-config confirmation: %v", err)
	}
	after, err := app.PreviewPurge(supervisor, project, scope)
	if err != nil || after.Revision != stale.Revision+1 {
		t.Fatalf("configuration revision = %d after %d: %v", after.Revision, stale.Revision, err)
	}
	if err := app.SetOperatingDuration(supervisor, project, "evidence-retention", 48*time.Hour); err != nil {
		t.Fatal(err)
	}
	unchanged, err := app.PreviewPurge(supervisor, project, scope)
	if err != nil || after.Confirmation != unchanged.Confirmation {
		t.Fatalf("identical config change invalidated fresh preview: %v", err)
	}
	if err := app.SetOperatingDuration(supervisor, project, "audit-retention", 72*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	now = fixtureTime.Add(48 * time.Hour)
	app, err = board.Open(path, board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	got, html, err := app.ReportHTML(supervisor, project, report.ID)
	if err != nil || !reflect.DeepEqual(got, report) || !bytes.Equal(html, originalHTML) {
		t.Fatalf("configuration changed independent historical report bytes: %v", err)
	}
	plan, err := app.PreviewPurge(supervisor, project, scope)
	if err != nil || len(plan.Ineligible) != 0 || len(plan.AuditSequences) != 0 ||
		!slices.Equal(plan.ReportIDs, []string{report.ID}) || !slices.Equal(plan.ReceiptIDs, []string{receipt.ID}) {
		t.Errorf("48h retention did not select candidate evidence alone: %+v, %v", plan, err)
	}
	now = fixtureTime.Add(72 * time.Hour)
	plan, err = app.PreviewPurge(supervisor, project, scope)
	if err != nil || !slices.Contains(plan.AuditSequences, oldEvents[0].Sequence) {
		t.Errorf("72h audit retention did not select event at its own boundary: %+v, %v", plan, err)
	}
}

func TestOperatingConfigurationControlsBackupHealthAndExpiry(t *testing.T) {
	now := fixtureTime
	app, _ := openBoard(t, func() time.Time { return now })
	directory := t.TempDir()
	first, err := app.Backup(supervisor, project, directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.SetOperatingDuration(supervisor, project, "backup-overdue", 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := app.SetOperatingDuration(supervisor, project, "backup-expiry", 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	now = fixtureTime.Add(2 * time.Hour)
	if app.Check().BackupOverdue {
		t.Error("configured strict health cutoff fired at equality")
	}
	now = now.Add(time.Microsecond)
	check := app.Check()
	health, err := app.Health(supervisor, project)
	if err != nil || !check.BackupOverdue || !strings.Contains(health.Alert, "2h0m0s") {
		t.Errorf("configured health cutoff not applied: check=%+v health=%+v err=%v", check, health, err)
	}
	keeper, err := app.Backup(supervisor, project, directory)
	if err != nil {
		t.Fatal(err)
	}
	now = fixtureTime.Add(3 * time.Hour)
	if count, err := app.ExpireBackups(supervisor, project, directory); err != nil || count != 1 {
		t.Errorf("configured expiry cutoff: removed=%d err=%v", count, err)
	}
	for _, suffix := range []string{"", ".sha256"} {
		if _, err := os.Stat(first.Path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expired backup file remains: %v", err)
		}
		if _, err := os.Stat(keeper.Path + suffix); err != nil {
			t.Errorf("ineligible backup did not survive: %v", err)
		}
	}
}

func TestOperatingConfigurationKeepsPendingExpiryAuthorized(t *testing.T) {
	now := fixtureTime
	app, path := openBoard(t, func() time.Time { return now })
	directory := t.TempDir()
	backup, err := app.Backup(supervisor, project, directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.SetOperatingDuration(supervisor, project, "backup-expiry", 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	sidecar := backup.Path + ".sha256"
	saved := filepath.Join(directory, "saved-checksum")
	if err := os.Rename(sidecar, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sidecar, 0700); err != nil {
		t.Fatal(err)
	}
	now = fixtureTime.Add(3 * time.Hour)
	if count, err := app.ExpireBackups(supervisor, project, directory); count != 0 || err == nil ||
		!strings.Contains(err.Error(), "non-file companion") {
		t.Fatalf("expected interruption after authorized intent: count=%d err=%v", count, err)
	}
	if _, err := os.Stat(backup.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expiry had not started deleting the backup: %v", err)
	}
	if err := app.SetOperatingDuration(supervisor, project, "backup-expiry", 48*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, sidecar); err != nil {
		t.Fatal(err)
	}
	app, err = board.Open(path, board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if count, err := app.ExpireBackups(supervisor, project, directory); count != 1 || err != nil {
		t.Fatalf("reopen did not complete the durable expiry under its original policy: count=%d err=%v", count, err)
	}
	if _, err := os.Stat(sidecar); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed expiry left its checksum: %v", err)
	}
}

func TestOperatingConfigurationRollsBackIfAuditCannotAppend(t *testing.T) {
	now := fixtureTime
	app, path := openBoard(t, func() time.Time { return now })
	plan, err := app.PreviewPurge(supervisor, project, nil)
	if err != nil {
		t.Fatal(err)
	}
	audit, err := app.Audit(supervisor, project)
	if err != nil || len(audit) == 0 {
		t.Fatalf("missing initial audit: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := bolt.Open(filepath.Clean(path), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A valid exhausted allocator permits custody reads but cannot
	// append the audit evidence required by this configuration change.
	fault := store.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("audit")).SetSequence(math.MaxUint64)
	})
	if err := errors.Join(fault, store.Close()); err != nil {
		t.Fatal(err)
	}
	open := func() *board.Board {
		t.Helper()
		reopened, err := board.Open(path, board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := reopened.Close(); err != nil {
				t.Error(err)
			}
		})
		return reopened
	}
	app = open()
	if err := app.SetOperatingDuration(supervisor, project, "backup-interval", 48*time.Hour); !errors.Is(err, board.ErrInvalid) ||
		!strings.Contains(err.Error(), "audit sequence capacity is exhausted") {
		t.Errorf("configuration write did not reach the terminal audit append: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	app = open()
	config, err := app.OperatingConfig(supervisor, project)
	if err != nil || config["backup-interval"] != 24*time.Hour {
		t.Errorf("failed audit transaction persisted config: %+v, %v", config, err)
	}
	retained, err := app.Audit(supervisor, project)
	if err != nil || !reflect.DeepEqual(retained, audit) {
		t.Fatalf("failed audit transaction changed retained audit: %v", err)
	}
	after, err := app.PreviewPurge(supervisor, project, nil)
	if err != nil || after.Revision != plan.Revision {
		t.Fatalf("failed audit transaction advanced data revision: %d → %d, %v", plan.Revision, after.Revision, err)
	}
}
