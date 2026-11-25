package board

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFollowupBackupRechecksOperatorAfterSnapshotStaging(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	const custodian = "backup-fixture-custodian"
	if err := f.app.SetAccount(secondActor, custodian, "synthetic-backup-fixture-account", []Role{Operator, Reviewer}); err != nil {
		t.Fatal(err)
	}
	path := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	var armed atomic.Bool
	writerReached, writerRelease := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(writerRelease) }) }
	settings := Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time {
		if armed.CompareAndSwap(true, false) {
			close(writerReached)
			<-writerRelease
		}
		return f.at
	}}
	app, err := Open(path, settings)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var jobs sync.WaitGroup
	t.Cleanup(func() {
		release()
		jobs.Wait()
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	removed := make(chan error, 1)
	armed.Store(true)
	jobs.Go(func() { removed <- app.RemoveAccount(custodian, secondActor) })
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	select {
	case <-writerReached:
	case err := <-removed:
		t.Fatalf("public account removal did not stage its audit: %v", err)
	case <-timer.C:
		t.Fatal("public removal did not reach its transaction clock")
	}
	type attempt struct {
		backup Backup
		err    error
	}
	snapshot := make(chan attempt, 1)
	jobs.Go(func() {
		backup, err := app.Backup(secondActor, secondProject, dir)
		snapshot <- attempt{backup, err}
	})
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	var staged string
	for staged == "" {
		select {
		case <-poll.C:
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if !strings.HasSuffix(entry.Name(), ".db.sha256") {
					continue
				}
				companion := filepath.Join(dir, entry.Name())
				if raw, err := os.ReadFile(companion); err == nil && len(raw) == 65 && raw[64] == '\n' {
					staged = strings.TrimSuffix(companion, ".sha256")
				}
			}
		case finished := <-snapshot:
			t.Fatalf("backup finished before authorization changed: %v", finished.err)
		case <-timer.C:
			t.Fatal("backup did not write its independent stage and companion under original authorization")
		}
	}
	release()
	select {
	case err := <-removed:
		if err != nil {
			t.Fatal(err)
		}
	case <-timer.C:
		t.Fatal("public removal did not commit after clock release")
	}
	select {
	case result := <-snapshot:
		if !errors.Is(result.err, ErrForbidden) || result.backup.Path != staged {
			t.Errorf("backup recorded success after the original operator was removed: %v", result.err)
		}
	case <-timer.C:
		t.Fatal("backup did not settle after the competing writer committed")
	}
	if health, err := app.Health(custodian, secondProject); err != nil || !health.LastBackup.IsZero() {
		t.Errorf("forbidden final write updated the recorded successful backup: %v", err)
	}
	events, err := app.Audit(custodian, secondProject)
	if err != nil || slices.ContainsFunc(events, func(event AuditEvent) bool {
		return event.Action == "backup.create" && event.Outcome == "success"
	}) || !slices.ContainsFunc(events, func(event AuditEvent) bool {
		return event.Action == "backup.create" && event.Outcome == "denied"
	}) {
		t.Errorf("authorization change did not preserve the denial audit: %v", err)
	}
	if r, raw, err := app.ReportHTML(custodian, secondProject, f.report.ID); err != nil || r != f.report || !bytes.Equal(raw, f.html) {
		t.Errorf("authorization change modified independently stored frozen HTML: %v", err)
	}
}
