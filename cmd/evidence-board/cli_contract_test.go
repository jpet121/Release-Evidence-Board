package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"release-evidence-board/internal/board"
)

const testCLIPassword = "synthetic-cli-operator-password"

func invokeCLI(command, path, secret string, flags ...string) (string, error) {
	args := append([]string{command, "-db", path}, flags...)
	var output bytes.Buffer
	err := run(args, strings.NewReader(secret), &output)
	return output.String(), err
}

func operatorCLI(command, path string, flags ...string) (string, error) {
	return invokeCLI(command, path, testCLIPassword+"\r\n", append([]string{"-actor", "reviewer"}, flags...)...)
}

func initializeCLI(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pilot.db")
	if _, err := operatorCLI("init", path, "-password-stdin"); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLIProcessEntryPoint(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name, output string
		args         []string
		exit         int
	}{
		{"without_command", "use init, serve, account", nil, 1},
		{"help", "Usage of serve:", []string{"serve", "-help"}, 0},
	} {
		t.Run(item.name, func(t *testing.T) {
			args := append([]string{"-test.run=^TestCLIExecutableFixture$", "--"}, item.args...)
			child := exec.CommandContext(t.Context(), executable, args...)
			child.Env = append(os.Environ(), "REB_TEST_RUN_CLI=1")
			raw, err := child.CombinedOutput()
			exit := 0
			if err != nil {
				var exited *exec.ExitError
				if !errors.As(err, &exited) {
					t.Fatal(err)
				}
				exit = exited.ExitCode()
			}
			if exit != item.exit || !bytes.Contains(raw, []byte(item.output)) {
				t.Fatalf("exit=%d output=%q, expected exit=%d containing %q", exit, raw, item.exit, item.output)
			}
		})
	}
}

func TestCLIExecutableFixture(t *testing.T) {
	if os.Getenv("REB_TEST_RUN_CLI") != "1" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		t.Fatal("CLI fixture requires an argument separator")
	}
	os.Args = append([]string{"evidence-board"}, os.Args[separator+1:]...)
	main()
}

func TestCLIRejectsIncompleteAndUnknownOperatorArguments(t *testing.T) {
	existing := initializeCLI(t)
	proposed := filepath.Join(t.TempDir(), "uninitialized.db")
	// If early validation fails, the real listener still rejects this port.
	badListener := []string{"-listen", "127.0.0.1:65536"}
	for _, item := range []struct {
		name, operation, path, message string
		flags                          []string
	}{
		{"unknown_flag", "serve", existing, "flag provided but not defined", append(slices.Clone(badListener), "-fixture")},
		{"extra_positional", "serve", existing, "positional arguments are unsupported", append(slices.Clone(badListener), "unbound")},
		{"restore_input", "restore", proposed, "restore requires -from", nil},
		{"missing_backup", "restore", proposed, "missing.db", []string{"-from", filepath.Join(t.TempDir(), "missing.db")}},
		{"missing_database", "serve", proposed, "open initialized evidence database", badListener},
		{"invalid_project", "init", proposed, "project and product require valid identifiers", []string{"-project", "invalid/path"}},
		{"missing_actor", "backup", existing, "provide -actor", nil},
		{"backup_destination", "backup", existing, "provide a protected -backups directory", []string{"-actor", "reviewer"}},
		{"expiry_destination", "expire-backups", existing, "provide a protected -backups directory", []string{"-actor", "reviewer"}},
		{"missing_expiry_directory", "expire-backups", existing, "absent-backups", []string{"-actor", "reviewer", "-backups", filepath.Join(t.TempDir(), "absent-backups")}},
		{"unknown_operation", "invented", existing, "unknown operation", []string{"-actor", "reviewer"}},
		{"unnamed_bootstrap", "init", filepath.Join(t.TempDir(), "named.db"), "init requires a named -actor", nil},
		{"backup_through_plain_file", "backup", existing, "pilot.db", []string{"-actor", "reviewer", "-backups", filepath.Join(existing, "child")}},
	} {
		t.Run(item.name, func(t *testing.T) {
			_, err := invokeCLI(item.operation, item.path, testCLIPassword+"\n", item.flags...)
			if err == nil || !strings.Contains(err.Error(), item.message) {
				t.Fatalf("operation %s produced %v, want %q", item.operation, err, item.message)
			}
		})
	}
	if _, err := os.Stat(proposed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a rejected operation created the uninitialized database: %v", err)
	}
}

func TestCLIGeneratedAccountCredentialsAndRevocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pilot.db")
	output, err := invokeCLI("init", path, "", "-actor", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	admin := issuedCLIPassword(t, output, "reviewer")
	issued, err := invokeCLI("account", path, admin+"\n", "-actor", "reviewer", "-account", "assistant", "-roles", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	viewer := issuedCLIPassword(t, issued, "assistant")
	if _, err := invokeCLI("backup", path, viewer+"\n", "-actor", "assistant", "-backups", t.TempDir()); !errors.Is(err, board.ErrForbidden) {
		t.Fatalf("issued viewer credentials enabled operator backup: %v", err)
	}
	if _, err := invokeCLI("account", path, admin+"\n", "-actor", "reviewer", "-account", "assistant", "-roles", "invalid-role"); !errors.Is(err, board.ErrInvalid) {
		t.Fatalf("invalid role entered the access roster: %v", err)
	}
	if _, err := invokeCLI("account", path, admin+"\n", "-actor", "reviewer", "-account", "assistant"); err != nil {
		t.Fatal(err)
	}
	if _, err := invokeCLI("backup", path, viewer+"\n", "-actor", "assistant", "-backups", t.TempDir()); !errors.Is(err, board.ErrForbidden) {
		t.Fatalf("revoked account still authenticates: %v", err)
	}
}

func issuedCLIPassword(t *testing.T, written, account string) string {
	t.Helper()
	secret, found := strings.CutPrefix(strings.TrimSuffix(written, "\n"), "Account "+account+" temporary display of generated password: ")
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if !found || err != nil || len(raw) != 24 {
		t.Fatal("generated credential did not follow its one-time display contract")
	}
	return secret
}

func TestCLIClosedCredentialInputIsReported(t *testing.T) {
	path := initializeCLI(t)
	closed := closedCLIFile(t)
	err := run([]string{"backup", "-db", path, "-actor", "reviewer", "-backups", t.TempDir()}, closed, io.Discard)
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("operator continued with a closed credential source: %v", err)
	}
	destination := filepath.Join(t.TempDir(), "unassigned.db")
	err = run([]string{"init", "-db", destination, "-actor", "reviewer", "-password-stdin"}, closed, io.Discard)
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("bootstrap continued with a closed credential source: %v", err)
	}
	if _, err := operatorCLI("init", destination, "-password-stdin"); err != nil {
		t.Fatalf("failed credential read assigned an operator: %v", err)
	}
}

func TestCLICredentialsReadThroughEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "eof.db")
	if _, err := invokeCLI("init", path, testCLIPassword, "-actor", "reviewer", "-password-stdin"); err != nil {
		t.Fatalf("bootstrap discarded valid credentials ending at EOF: %v", err)
	}
	if _, err := invokeCLI("backup", path, testCLIPassword, "-actor", "reviewer", "-backups", t.TempDir()); err != nil {
		t.Fatalf("operator discarded valid credentials ending at EOF: %v", err)
	}
}

func closedCLIFile(t *testing.T) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "closed-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestCLIServeChecksTLSAndScheduledAssignment(t *testing.T) {
	path := initializeCLI(t)
	if _, err := operatorCLI("account", path, "-account", "reader", "-roles", "viewer"); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		name, errorText string
		flags           []string
	}{
		{"malformed_address", "missing port", []string{"-listen", "127.0.0.1"}},
		{"missing_key", "provide both TLS", []string{"-tls-cert", "fixture.pem"}},
		{"missing_certificate", "provide both TLS", []string{"-tls-key", "fixture.pem"}},
		{"missing_scheduled_actor", "scheduled backups require both", []string{"-backups", t.TempDir()}},
		{"missing_scheduled_directory", "scheduled backups require both", []string{"-backup-operator", "reviewer"}},
		{"unknown_scheduled_actor", "record is unavailable", []string{"-backups", t.TempDir(), "-backup-operator", "unknown"}},
		{"unauthorized_scheduled_actor", "this role cannot perform the action", []string{"-backups", t.TempDir(), "-backup-operator", "reader"}},
	} {
		t.Run(check.name, func(t *testing.T) {
			_, err := invokeCLI("serve", path, "", check.flags...)
			if err == nil || !strings.Contains(err.Error(), check.errorText) {
				t.Fatalf("startup configuration produced %v, want %q", err, check.errorText)
			}
		})
	}
	cert := filepath.Join(t.TempDir(), "missing-cert.pem")
	output, err := invokeCLI("serve", path, "", "-listen", "127.0.0.1:0", "-tls-cert", cert, "-tls-key", cert)
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(output, "https://127.0.0.1:0") {
		t.Fatalf("unusable TLS setup did not fail clearly: %v", err)
	}
}

func TestCLIListenerConflictJoinsScheduledInitialBackup(t *testing.T) {
	path := initializeCLI(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	}()
	backups := t.TempDir()
	_, err = invokeCLI("serve", path, "", "-listen", listener.Addr().String(), "-backups", backups, "-backup-operator", "reviewer")
	var operation *net.OpError
	if !errors.As(err, &operation) || operation.Op != "listen" {
		t.Fatalf("occupied port did not produce a listen error: %v", err)
	}
	files, err := os.ReadDir(backups)
	if err != nil || len(files) != 2 {
		t.Fatalf("startup did not join its scheduled backup attempt: %d files, %v", len(files), err)
	}
	_, err = invokeCLI("serve", path, "", "-listen", listener.Addr().String(), "-backups", backups, "-backup-operator", "reviewer")
	if !errors.As(err, &operation) || operation.Op != "listen" {
		t.Fatalf("occupied port did not produce a listen error: %v", err)
	}
	after, err := os.ReadDir(backups)
	if err != nil || len(after) != 2 || files[0].Name() != after[0].Name() || files[1].Name() != after[1].Name() {
		t.Fatalf("startup duplicated a recent backup: %d files, %v", len(after), err)
	}
}

func TestCLIReportsUnverifiedRestoreAndClosedResultOutput(t *testing.T) {
	path := initializeCLI(t)
	raw, err := operatorCLI("backup", path, "-backups", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var backup board.Backup
	if err := json.Unmarshal([]byte(raw), &backup); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "restored-valid.db")
	args := []string{"restore", "-db", destination, "-from", backup.Path}
	if err := run(args, strings.NewReader(""), closedCLIFile(t)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("CLI claimed it wrote the restore result to a closed destination: %v", err)
	}
	if err := os.Remove(backup.Path + ".sha256"); err != nil {
		t.Fatal(err)
	}
	destination = filepath.Join(t.TempDir(), "restored-unverified.db")
	output, err := invokeCLI("restore", destination, "", "-from", backup.Path)
	var result board.RestoreResult
	decodeError := json.Unmarshal([]byte(output), &result)
	if !errors.Is(err, board.ErrIntegrity) || decodeError != nil || result.IntegrityOK {
		t.Fatalf("missing checksum did not produce an unverified restore: %v, %v", err, decodeError)
	}
	if _, err := operatorCLI("revalidate", destination); !errors.Is(err, board.ErrIntegrity) {
		t.Fatalf("CLI revalidation lifted a corrupt backup hold: %v", err)
	}
}

func TestCLIMaintenanceDeduplicatesOverdueAlertOnLaterTick(t *testing.T) {
	var microseconds atomic.Int64
	microseconds.Store(time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC).UnixMicro())
	clock := func() time.Time { return time.UnixMicro(microseconds.Load()).UTC() }
	app, err := board.Open(filepath.Join(t.TempDir(), "pilot.db"),
		board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := app.Bootstrap("reviewer", testCLIPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Backup("reviewer", "pilot", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	microseconds.Add((27 * time.Hour).Microseconds())
	logs := captureMaintenanceLogs(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		exited := make(chan struct{})
		go func() {
			defer close(exited)
			maintenance(ctx, app, "", "")
		}()
		synctest.Wait()
		if strings.Count(logs.String(), "more than 26 hours old") != 1 {
			t.Fatal("initial overdue backup did not emit one alert")
		}
		microseconds.Add(time.Minute.Microseconds())
		time.Sleep(time.Minute)
		synctest.Wait()
		health, err := app.Health("reviewer", "pilot")
		if err != nil || !health.CheckedAt.Equal(clock()) || !health.BackupOverdue {
			t.Fatalf("subsequent tick lost health state: %v", err)
		}
		if strings.Count(logs.String(), "more than 26 hours old") != 1 {
			t.Fatal("second health-check tick repeated an unresolved overdue alert")
		}
		cancel()
		<-exited
	})
}

func TestCLIMaintenanceAlertsWhenRealStoreBecomesUnavailable(t *testing.T) {
	path := initializeCLI(t)
	app, err := board.Open(path, board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	logs := captureMaintenanceLogs(t)
	for _, expected := range []int{1, 2} {
		result := app.Check()
		if result.DatabaseOK || result.ConsecutiveFailures != expected {
			t.Fatalf("closed store health showed %d failures and availability %t", result.ConsecutiveFailures, result.DatabaseOK)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	maintenance(ctx, app, t.TempDir(), "reviewer")
	if !strings.Contains(logs.String(), "three consecutive application health failures") ||
		!strings.Contains(logs.String(), "scheduled backup check failed") {
		t.Fatal("unavailable datastore failed to produce both monitoring alerts")
	}
}

type maintenanceLogCapture struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (logs *maintenanceLogCapture) Write(p []byte) (int, error) {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	return logs.buffer.Write(p)
}

func (logs *maintenanceLogCapture) String() string {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	return logs.buffer.String()
}

func captureMaintenanceLogs(t *testing.T) *maintenanceLogCapture {
	t.Helper()
	logs := &maintenanceLogCapture{}
	previous := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	return logs
}

func TestCLIViewerCannotPreviewPurge(t *testing.T) {
	path := initializeCLI(t)
	printed, err := operatorCLI("account", path, "-account", "reader", "-roles", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	reader := issuedCLIPassword(t, printed, "reader")
	output, err := invokeCLI("purge", path, reader+"\n", "-actor", "reader")
	if !errors.Is(err, board.ErrForbidden) || output != "" {
		t.Fatalf("viewer obtained operator purge result: %v", err)
	}
}

func TestCLIRetentionDrillAndIntegrityHold(t *testing.T) {
	path := initializeCLI(t)
	backups := t.TempDir()
	clock := time.Now().UTC().Add(-181 * 24 * time.Hour)
	opts := board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return clock }}
	app, err := board.Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	roster := board.Baseline{ID: "B1", Revision: 1, ProductID: "DEMO", Behaviors: []board.Behavior{{
		ID: "B1", Revision: 1, Required: true, Text: "Retention fixture",
		Policy: board.Policy{ID: "P1", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
		Tests:  []board.TestPlan{{ID: "T1", ExpectedOutcome: "Retain while in scope", MustPass: true, PlannedAttempts: 1}},
	}}}
	draft, err := app.CreateDraft("reviewer", "pilot", roster, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := app.Approve("reviewer", "pilot", draft.ID, draft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	newCandidate := func(mark string) string {
		t.Helper()
		c, reused, err := app.CreateCandidate("reviewer", "pilot", board.CandidateInput{BaselineDigest: approved.Descriptor.Digest,
			Manifest: board.Manifest{ProductID: "DEMO", ReleaseID: "release-" + mark, ApplicationVersion: "1",
				BuildDigest: strings.Repeat(mark, 64), PromptDigest: "not_applicable", ModelConfigDigest: "not_applicable",
				KnowledgeDigest: "not_applicable", EnvironmentID: "E1"}})
		if err != nil || reused {
			t.Fatalf("provision retention candidate: %v", err)
		}
		return c.ID
	}
	old := []string{newCandidate("a"), newCandidate("b")}
	aged, err := app.Backup("reviewer", "pilot", backups)
	if err != nil {
		t.Fatal(err)
	}
	clock = time.Now().UTC()
	retained := newCandidate("c")
	recent, err := app.Backup("reviewer", "pilot", backups)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "drill.db")
	if _, err := invokeCLI("restore", restored, "", "-from", recent.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := operatorCLI("restore-drill", path, "-from", restored); err != nil {
		t.Fatal(err)
	}
	if _, err := operatorCLI("suspend", path); err != nil {
		t.Fatal(err)
	}
	held, err := board.Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	_, assessError := held.Assess("reviewer", "pilot", retained)
	err = held.Close()
	if !errors.Is(assessError, board.ErrIntegrity) || err != nil {
		t.Fatalf("operator CLI suspension was ineffective: assessment=%v close=%v", assessError, err)
	}
	if _, err := operatorCLI("revalidate", path); err != nil {
		t.Fatal(err)
	}
	scope := strings.Join(old, ",")
	serialized, err := operatorCLI("purge", path, "-candidates", scope)
	if err != nil {
		t.Fatal(err)
	}
	var preview board.PurgePlan
	if err := json.Unmarshal([]byte(serialized), &preview); err != nil || len(preview.CandidateIDs) != 2 || len(preview.Ineligible) > 0 ||
		!preview.ExternalCopiesNotPurged || preview.Confirmation == "" {
		t.Fatalf("retention preview differs from exact eligible scope: %v", err)
	}
	if _, err := operatorCLI("purge", path, "-candidates", retained, "-confirm", preview.Confirmation); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("changed scope was purged using another confirmation: %v", err)
	}
	if _, err := operatorCLI("purge", path, "-candidates", scope, "-confirm", preview.Confirmation); err != nil {
		t.Fatal(err)
	}
	expired, err := operatorCLI("expire-backups", path, "-backups", backups)
	if err != nil || !strings.Contains(expired, "Removed 1 expired backup sets.") {
		t.Fatalf("CLI backup expiry result %q, %v", expired, err)
	}
	if _, err := os.Stat(aged.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aged backup remains: %v", err)
	}
	if _, err := os.Stat(recent.Path); err != nil {
		t.Fatalf("expiry removed fresh backup: %v", err)
	}
	app, err = board.Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	}()
	candidates, err := app.Candidates("reviewer", "pilot")
	if err != nil || len(candidates) != 1 || candidates[0].ID != retained {
		t.Fatalf("confirmed purge changed out-of-scope candidates: %v", err)
	}
	health, err := app.Health("reviewer", "pilot")
	if err != nil || health.LastDrill.IsZero() {
		t.Fatalf("CLI restore drill did not record proof completion: %v", err)
	}
}
