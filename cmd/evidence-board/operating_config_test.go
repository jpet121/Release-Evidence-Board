package main

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestCLIOperatingConfigurationAudit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pilot.db")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(secret, operation, actor string, flags ...string) (string, int) {
		t.Helper()
		args := []string{"-test.run=^TestCLIExecutableFixture$", "--", operation, "-db", path, "-actor", actor}
		child := exec.CommandContext(t.Context(), executable, append(args, flags...)...)
		child.Env = append(os.Environ(), "REB_TEST_RUN_CLI=1")
		child.Stdin = strings.NewReader(secret + "\n")
		output, err := child.CombinedOutput()
		if err == nil {
			payload, ok := cliTestOutput(string(output), testing.CoverMode() != "")
			if !ok {
				t.Fatalf("unexpected successful CLI fixture transcript: %q", output)
			}
			return payload, 0
		}
		var exited *exec.ExitError
		if !errors.As(err, &exited) {
			t.Fatal(err)
		}
		return string(output), exited.ExitCode()
	}
	if output, exit := invoke(testCLIPassword, "init", "reviewer", "-password-stdin"); exit != 0 {
		t.Fatalf("initialize real CLI store: exit=%d output=%q", exit, output)
	}
	readConfig := func() map[string]string {
		t.Helper()
		output, exit := invoke(testCLIPassword, "config", "reviewer")
		var config map[string]string
		if exit != 0 || json.Unmarshal([]byte(output), &config) != nil {
			t.Fatalf("read operating configuration through authenticated CLI: exit=%d output=%q", exit, output)
		}
		return config
	}
	before := readConfig()
	if before["backup-interval"] != "24h0m0s" || before["backup-overdue"] != "26h0m0s" ||
		before["backup-expiry"] != "720h0m0s" || before["evidence-retention"] != "4320h0m0s" ||
		before["audit-retention"] != "4320h0m0s" {
		t.Fatalf("resolved operating defaults = %v", before)
	}

	output, exit := invoke(testCLIPassword, "account", "reviewer", "-account", "only-viewer", "-roles", "viewer")
	if exit != 0 {
		t.Fatalf("add Viewer fixture: exit=%d output=%q", exit, output)
	}
	viewerPassword := issuedCLIPassword(t, output, "only-viewer")
	output, exit = invoke(viewerPassword, "config", "only-viewer")
	if exit != 1 || !strings.Contains(output, board.ErrForbidden.Error()) || strings.Contains(output, "backup-interval") {
		t.Fatalf("Viewer configuration read: exit=%d output=%q", exit, output)
	}
	change := []string{"-setting", "backup-interval", "-duration", "48h"}
	output, exit = invoke(viewerPassword, "config", "only-viewer", change...)
	if exit != 1 || !strings.Contains(output, board.ErrForbidden.Error()) {
		t.Fatalf("Viewer configuration change: exit=%d output=%q", exit, output)
	}
	output, exit = invoke(testCLIPassword, "config", "reviewer", "-setting", "backup-interval", "-duration", "-1h")
	if exit != 1 || !strings.Contains(output, board.ErrInvalid.Error()) {
		t.Fatalf("negative duration: exit=%d output=%q", exit, output)
	}
	output, exit = invoke(testCLIPassword, "config", "reviewer", "-setting", "backup-interval", "-duration", "9223372036854775808ns")
	if exit != 1 || !strings.Contains(output, board.ErrInvalid.Error()) {
		t.Fatalf("unrepresentable duration: exit=%d output=%q", exit, output)
	}
	output, exit = invoke(testCLIPassword, "config", "reviewer", "-setting", "max-upload", "-duration", "1h")
	if exit != 1 || !strings.Contains(output, board.ErrInvalid.Error()) {
		t.Fatalf("unsupported setting: exit=%d output=%q", exit, output)
	}
	if got := readConfig()["backup-interval"]; got != "24h0m0s" {
		t.Fatalf("rejected change persisted: backup interval=%s", got)
	}
	started := time.Now().UTC().Truncate(time.Microsecond)
	for range 2 {
		if output, exit := invoke(testCLIPassword, "config", "reviewer", change...); exit != 0 {
			t.Fatalf("set operating configuration: exit=%d output=%q", exit, output)
		}
	}
	if got := readConfig()["backup-interval"]; got != "48h0m0s" {
		t.Fatalf("operator change did not survive CLI restart: %s", got)
	}
	app, err := board.Open(path, board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	audit, err := app.Audit("reviewer", "pilot")
	if err != nil {
		t.Fatal(err)
	}
	var changes []board.AuditEvent
	for _, event := range audit {
		if event.Action == "config.change" && event.Outcome == "success" {
			changes = append(changes, event)
		}
	}
	want := []string{"backup-interval", "old=24h0m0s", "new=48h0m0s"}
	if len(changes) != 1 || changes[0].Actor != "reviewer" || changes[0].Time.Location() != time.UTC ||
		changes[0].Time.Before(started) || changes[0].Time.After(time.Now().UTC()) || !slices.Equal(changes[0].References, want) {
		t.Fatalf("durable attributed config audit = %#v, want one change with %v", changes, want)
	}
	t.Log(`REB_ACCEPTANCE_INSTANCE {"id":"AC-012-01.P005","case_id":"AC-012-01","parameters":{"authorizedAction":"config change"},"actual":"Actor, UTC time, action, references and outcome recorded"}`)
}

var cliCoverageTrailer = regexp.MustCompile(`^coverage: (100\.0|[0-9]{1,2}\.[0-9])% of statements( in [A-Za-z0-9_./-]+(, [A-Za-z0-9_./-]+)*)?\n$`)

// The actual CLI runs in the child test executable so coverage and mutation
// instrumentation remain active. Only the known testing footer is framing.
func cliTestOutput(raw string, instrumented bool) (string, bool) {
	if instrumented {
		const boundary = "\ncoverage: "
		at := strings.LastIndex(raw, boundary)
		if at < 0 || !cliCoverageTrailer.MatchString(raw[at+1:]) {
			return "", false
		}
		raw = raw[:at+1]
	}
	payload, ok := strings.CutSuffix(raw, "PASS\n")
	return payload, ok && (payload == "" || strings.HasSuffix(payload, "\n"))
}

func TestCLITestOutputFraming(t *testing.T) {
	const payload = "{\"backup-interval\":\"24h0m0s\"}\n"
	for _, row := range []struct {
		name, raw        string
		instrumented, ok bool
	}{
		{"plain", payload + "PASS\n", false, true},
		{"instrumented", payload + "PASS\ncoverage: 7.3% of statements in ./...\n", true, true},
		{"wrong mode", payload + "PASS\ncoverage: 7.3% of statements in ./...\n", false, false},
		{"missing pass", payload + "coverage: 7.3% of statements in ./...\n", true, false},
		{"partial pass line", payload + "NOTPASS\n", false, false},
		{"bad coverage", payload + "PASS\ncoverage: 999.0% of statements\n", true, false},
		{"extra footer", payload + "PASS\ncoverage: 7.3% of statements\nignored\n", true, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			decoded, ok := cliTestOutput(row.raw, row.instrumented)
			if ok != row.ok || ok && decoded != payload {
				t.Fatalf("decoded %q, accepted %t; expected acceptance %t", decoded, ok, row.ok)
			}
		})
	}
}

func TestCLIOperatingBackupSchedule(t *testing.T) {
	path := initializeCLI(t)
	backups := t.TempDir()
	when := time.Now().UTC().Add(-36 * time.Hour)
	app, err := board.Open(path, board.Settings{
		ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return when },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Backup("reviewer", "pilot", backups); err != nil {
		_ = app.Close()
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	// A conflicting listener makes serve join its real initial maintenance
	// pass immediately, without waiting for a later wall-clock tick.
	for _, sample := range []struct {
		interval string
		files    int
	}{{"48h", 2}, {"12h", 4}} {
		_, err := operatorCLI("config", path, "-setting", "backup-interval", "-duration", sample.interval)
		if err != nil {
			t.Fatal(err)
		}
		_, err = invokeCLI("serve", path, "", "-listen", listener.Addr().String(),
			"-backups", backups, "-backup-operator", "reviewer")
		var listenError *net.OpError
		if !errors.As(err, &listenError) || listenError.Op != "listen" {
			t.Fatalf("expected owned listener conflict after maintenance, got %v", err)
		}
		entries, err := os.ReadDir(backups)
		if err != nil || len(entries) != sample.files {
			t.Fatalf("scheduled backup with interval %s: got %d files, want %d (%v)", sample.interval, len(entries), sample.files, err)
		}
	}
}
