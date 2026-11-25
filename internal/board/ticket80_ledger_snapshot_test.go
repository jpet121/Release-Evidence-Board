package board_test

import (
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestTicket80EmptyCandidateKeepsProductWideLedgerInAssessmentAndReport(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	first := fixtureCandidate(t, app, vectorBaseline())
	for i, run := range []string{"FIRST", "SECOND"} {
		stamp := "2026-11-17T11:00:00Z"
		receipt := importBytes(t, app, first.ID, csvBytes(t,
			resultRow(first, "T1", "1", run, run+"-RESULT", stamp, stamp, "PASS")))
		if receipt.LedgerHighWater != uint64(i+1) {
			t.Fatal("fixture did not commit two separate product-wide sequences")
		}
	}
	manifest := vectorManifest()
	manifest.BuildDigest = strings.Repeat("b", 64)
	empty, reused, err := app.CreateCandidate(supervisor, project, board.CandidateInput{
		Manifest: manifest, BaselineDigest: first.BaselineDigest,
	})
	if err != nil || reused || empty.ID == first.ID {
		t.Fatalf("disjoint candidate unavailable: %v", err)
	}
	assessment, err := app.Assess(supervisor, project, empty.ID)
	if err != nil || assessment.LedgerHighWater != 2 || assessment.Status != board.Blocked ||
		len(assessment.Behaviors) != 1 || len(assessment.Behaviors[0].Runs) != 0 {
		t.Fatalf("empty candidate lost global ledger or inherited foreign evidence: %+v, %v", assessment, err)
	}
	report, err := app.Freeze(supervisor, project, empty.ID)
	if err != nil || report.LedgerHighWater != 2 || report.Status != board.Blocked {
		t.Fatalf("frozen report lost global high-water or became sufficient: %+v, %v", report, err)
	}
	_, html, err := app.ReportHTML(supervisor, project, report.ID)
	if err != nil || !strings.Contains(string(html), "Ledger high-water mark 2") ||
		!strings.Contains(string(html), "MISSING_RUN") {
		t.Fatalf("offline report does not retain global ledger and missing evidence: %v", err)
	}
}
