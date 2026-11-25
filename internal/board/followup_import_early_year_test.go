package board_test

import (
	"bytes"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupImportChecksOnlyPresentResultFinishTimes(t *testing.T) {
	for _, trial := range []struct {
		name         string
		now          time.Time
		finish       string
		runOnly      bool
		wantClockErr bool
	}{
		{"year_zero_run_only", time.Date(0, time.July, 1, 0, 0, 0, 0, time.UTC), "0000-07-01T00:00:00Z", true, false},
		{"year_one_at_boundary", time.Date(0, time.December, 31, 23, 55, 0, 0, time.UTC), "0001-01-01T00:00:00Z", false, false},
		{"year_one_beyond_boundary", time.Date(0, time.December, 31, 23, 54, 59, 0, time.UTC), "0001-01-01T00:00:00Z", false, true},
	} {
		t.Run(trial.name, func(t *testing.T) {
			app, _ := openBoard(t, func() time.Time { return trial.now })
			candidate := fixtureCandidate(t, app, vectorBaseline())
			var row []string
			if trial.runOnly {
				row = runOnly(candidate, "EARLY-RUN", trial.finish)
			} else {
				row = resultRow(candidate, "T1", "1", "EARLY-RUN", "EARLY-RESULT", trial.finish, trial.finish, "PASS")
			}
			raw := csvBytes(t, row)
			if rows, issues := board.ParseResultsCSV(raw); len(rows) != 1 || len(issues) != 0 {
				t.Fatalf("fixture is not valid under the public CSV grammar: %+v", issues)
			}
			preview, err := app.PreviewImport(supervisor, project, candidate.ID, raw)
			if trial.wantClockErr {
				if err != nil || preview.Token != "" || len(preview.Issues) != 1 || preview.Issues[0].Code != "CLOCK_SKEW_LIMIT" {
					t.Fatalf("a real timestamp past the five-minute boundary was not refused: %+v %v", preview.Issues, err)
				}
				return
			}
			if err != nil || preview.Token == "" || len(preview.Issues) != 0 {
				t.Fatalf("valid early-calendar upload did not produce a usable preview: %+v %v", preview.Issues, err)
			}
			receipt, err := app.CommitImport(supervisor, project, preview.Token, raw)
			if err != nil || receipt.ID == "" || receipt.Rows != 1 || receipt.LedgerHighWater != 1 {
				t.Fatalf("valid early-calendar upload did not commit its single run: %+v %v", receipt, err)
			}
			if stored, err := app.SourceCSV(supervisor, project, receipt.ID); err != nil || !bytes.Equal(stored, raw) {
				t.Fatalf("committed calendar boundary upload changed its original bytes: %v", err)
			}
		})
	}
}
