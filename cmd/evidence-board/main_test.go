package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestCLIAccountAndOperatorRecovery(t *testing.T) {
	const password = "synthetic-cli-password"
	work := t.TempDir()
	file := filepath.Join(work, "evidence.db")
	backups := filepath.Join(work, "backups")
	runCLI := func(db, operation, input string, flags ...string) (string, error) {
		t.Helper()
		args := append([]string{operation, "-db", db}, flags...)
		var out bytes.Buffer
		err := run(args, strings.NewReader(input), &out)
		return out.String(), err
	}
	first := []string{"-actor", "reviewer", "-password-stdin"}
	if _, err := runCLI(file, "init", password+"\n", first...); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(file, "init", password+"\n", first...); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("duplicate init = %v, want conflict", err)
	}
	printed, err := runCLI(file, "account", password+"\n", "-actor", "reviewer", "-account", "alice", "-roles", "contributor")
	if err != nil || !strings.Contains(printed, "Account alice") || strings.Contains(printed, password) {
		t.Fatalf("account output = %q, error %v", printed, err)
	}
	if _, err := runCLI(file, "backup", "wrong\n", "-actor", "reviewer", "-backups", backups); !errors.Is(err, board.ErrForbidden) {
		t.Fatalf("backup with wrong password = %v", err)
	}
	raw, err := runCLI(file, "backup", password+"\n", "-actor", "reviewer", "-backups", backups)
	if err != nil {
		t.Fatal(err)
	}
	var record board.Backup
	if err := json.Unmarshal([]byte(raw), &record); err != nil || len(record.SHA256) != 64 {
		t.Fatalf("backup info = %q, %v", raw, err)
	}
	destination := filepath.Join(work, "new.db")
	raw, err = runCLI(destination, "restore", "", "-from", record.Path)
	var restored board.RestoreResult
	parseError := json.Unmarshal([]byte(raw), &restored)
	if err != nil || parseError != nil || !restored.IntegrityOK {
		t.Fatalf("restore result = %q, command %v, parse %v", raw, err, parseError)
	}
	if _, err := runCLI(destination, "revalidate", password+"\n", "-actor", "reviewer"); err != nil {
		t.Fatal(err)
	}
	_, err = runCLI(destination, "serve", "", "-listen", "0.0.0.0:12345")
	if err == nil || !strings.Contains(err.Error(), "private HTTPS") {
		t.Fatalf("plain HTTP bound outside loopback: %v", err)
	}
}

func TestSTD03ClosedStartupOutputStopsBeforeOwnershipBegins(t *testing.T) {
	app, err := board.Open(filepath.Join(t.TempDir(), "evidence.db"), board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	}()
	out, err := os.CreateTemp(t.TempDir(), "closed-output-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if err := start(app, "127.0.0.1:0", "", "", "", "", out); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("startup after closed output = %v", err)
	}
}

func TestSTD05UnscheduledBackupOverdueMonitoring(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	app, err := board.Open(filepath.Join(t.TempDir(), "evidence.db"),
		board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := app.Bootstrap("reviewer", "synthetic-monitoring-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Backup("reviewer", "pilot", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now = now.Add(26 * time.Hour)
	maintenance(ctx, app, "", "")
	if strings.Contains(logs.String(), "more than 26 hours old") {
		t.Fatal("backup alert fired at equality")
	}
	now = now.Add(time.Microsecond)
	maintenance(ctx, app, "", "")
	if !strings.Contains(logs.String(), "more than 26 hours old") {
		t.Fatal("overdue backup alert missing without a scheduled backup operator")
	}
}
