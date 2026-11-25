package browser_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

var interruptionFixtureTime = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

const interruptedAIPath = "REB_AI_OFF_INTERRUPTION_PATH"
const interruptedAIMode = "REB_AI_OFF_INTERRUPTION_MODE"

func TestAIOffInterruptedImportRestartsWithExactPriorHistory(t *testing.T) {
	for _, trial := range []struct {
		mode, marker, status string
		exit                 int
		ledger               uint64
	}{
		{"rollback", "with private history retained: import audit transaction reached", "SUFFICIENT", 74, 1},
		{"committed", "with private history retained: import and audit committed", "BLOCKED", 75, 2},
	} {
		t.Run(trial.mode, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.db")
			id := seedHistoricalAI(t, source, interruptionFixtureTime, nil)
			settings := board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return interruptionFixtureTime }}
			app, err := board.Open(source, settings)
			if err != nil {
				t.Fatal(err)
			}
			backup, backupErr := app.Backup("reviewer", "pilot", filepath.Join(dir, "backups"))
			if err := errors.Join(backupErr, app.Close()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "restored.db")
			restored, err := board.RestoreBackup(backup.Path, path, settings)
			if err != nil || !restored.IntegrityOK {
				t.Fatalf("required prior history did not restore: %+v, %v", restored, err)
			}
			app, err = board.Open(path, settings)
			if err != nil {
				t.Fatal(err)
			}
			if err := app.Revalidate("reviewer", "pilot"); err != nil {
				_ = app.Close()
				t.Fatal(err)
			}
			prior, err := app.Assess("reviewer", "pilot", id)
			if err != nil || prior.Status != "SUFFICIENT" || prior.LedgerHighWater != 1 {
				_ = app.Close()
				t.Fatalf("prior fixture not sufficient: %+v, %v", prior, err)
			}
			receipt := prior.Behaviors[0].Runs[0].ReceiptID
			priorSource, err := app.SourceCSV("reviewer", "pilot", receipt)
			if err != nil {
				_ = app.Close()
				t.Fatal(err)
			}
			report, err := app.Freeze("reviewer", "pilot", id)
			if err != nil {
				_ = app.Close()
				t.Fatal(err)
			}
			_, originalHTML, err := app.ReportHTML("reviewer", "pilot", report.ID)
			if err := errors.Join(err, app.Close()); err != nil {
				t.Fatal(err)
			}

			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(t.Context(), executable, "-test.run=^TestAIOffImportInterruptionChild$")
			child.Env = append(os.Environ(), interruptedAIPath+"="+path, interruptedAIMode+"="+trial.mode)
			out, err := child.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != trial.exit || !strings.Contains(string(out), trial.marker) {
				t.Fatalf("owned child did not exit at expected material boundary: %v, %s", err, out)
			}
			app, err = board.Open(path, settings)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := app.Close(); err != nil {
					t.Error(err)
				}
			}()
			health, err := app.Health("reviewer", "pilot")
			if err != nil || health.HistoricalAI == nil || len(health.HistoricalAI.Requests) != 1 ||
				health.HistoricalAI.Requests[0].PayloadSHA256 != historyPayloadSHA ||
				health.HistoricalAI.Requests[0].ResponseSHA256 != historyResponseSHA || !health.HistoricalAI.Exhausted {
				t.Fatalf("native restart lost admitted AI-off history or accounting: %v", err)
			}
			assessment, err := app.Assess("reviewer", "pilot", id)
			if err != nil || assessment.Status != trial.status || assessment.LedgerHighWater != trial.ledger {
				t.Fatalf("native restart exposed the wrong import transaction outcome: %+v, %v", assessment, err)
			}
			original, err := app.SourceCSV("reviewer", "pilot", receipt)
			if err != nil || !bytes.Equal(original, priorSource) {
				t.Fatalf("native restart changed committed producer bytes: %v", err)
			}
			saved, html, err := app.ReportHTML("reviewer", "pilot", report.ID)
			if err != nil || !reflect.DeepEqual(saved, report) || !bytes.Equal(html, originalHTML) {
				t.Fatalf("native restart changed frozen evidence: %v", err)
			}
			candidate, _, _, err := app.Candidate("reviewer", "pilot", id)
			if err != nil {
				t.Fatal(err)
			}
			proposedID := "I-" + fixtureHash(interruptedAIFile(candidate))
			proposed, err := app.Receipt("reviewer", "pilot", proposedID)
			if trial.mode == "rollback" && !errors.Is(err, board.ErrNotFound) ||
				trial.mode == "committed" && (err != nil || proposed.LedgerHighWater != 2) {
				t.Fatalf("receipt visibility disagrees with native commit boundary: %+v, %v", proposed, err)
			}
			t.Logf("joined exit=%d ledger=%d case=%s history_sha256=%s", trial.exit, trial.ledger, trial.mode, historyPayloadSHA)
		})
	}
}

func TestAIOffImportInterruptionChild(t *testing.T) {
	path := os.Getenv(interruptedAIPath)
	if path == "" {
		return
	}
	mode := os.Getenv(interruptedAIMode)
	armed, calls := false, 0
	clock := func() time.Time {
		if armed {
			calls++
			if calls == 2 {
				fmt.Println("with private history retained: import audit transaction reached")
				os.Exit(74)
			}
		}
		return interruptionFixtureTime
	}
	app, err := board.Open(path, board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := app.Candidates("reviewer", "pilot")
	if err != nil || len(candidates) != 1 {
		t.Fatalf("child missing restored candidate: %v", err)
	}
	candidate := candidates[0]
	raw := interruptedAIFile(candidate)
	preview, err := app.PreviewImport("reviewer", "pilot", candidate.ID, raw)
	if err != nil || preview.Token == "" {
		t.Fatalf("child could not prepare the valid failure import: %v", err)
	}
	armed = mode == "rollback"
	receipt, err := app.CommitImport("reviewer", "pilot", preview.Token, raw)
	if armed || err != nil || receipt.LedgerHighWater != 2 {
		t.Fatalf("child missed its material commit boundary: %v", err)
	}
	fmt.Println("with private history retained: import and audit committed")
	os.Exit(75)
}

func interruptedAIFile(candidate board.Candidate) []byte {
	at := board.FormatUTC(interruptionFixtureTime.Add(-time.Minute))
	return []byte(followupHeader + fmt.Sprintf("reb-results/1.3,RESULT,RES-INTERRUPTED,DEMO,HISTORY,%s,%s,HISTORY,1,P-HISTORY,1,T-HISTORY,RUN-INTERRUPTED,1,%s,%s,FAIL,,\n",
		candidate.ManifestDigest, candidate.BaselineDigest, at, at))
}
