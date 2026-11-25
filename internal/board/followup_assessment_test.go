package board_test

import (
	"bytes"
	"slices"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

// BR07–10, D18 and AC-010. Both results count, including the earlier finish
// attached to the latter test. Optional evidence stays visible outside the
// required counts. The report takes a fresh assessment at the boundary.
func TestFollowupOldestPlannedResultControlsLiveAndFrozenFreshness(t *testing.T) {
	now := fixtureTime
	app, _ := openBoard(t, func() time.Time { return now })
	baseline := vectorBaseline()
	baseline.Behaviors[0].Policy.MaxAgeHours = 1
	baseline.Behaviors[0].Tests = append(baseline.Behaviors[0].Tests,
		board.TestPlan{ID: "T2", ExpectedOutcome: "Source is recorded", PlannedAttempts: 1})
	baseline.Behaviors = append(baseline.Behaviors, board.Behavior{
		ID: "BEH2", Revision: 1, Text: "Optional producer output check",
		Policy: board.Policy{ID: "POL2", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 1},
		Tests:  []board.TestPlan{{ID: "T3", ExpectedOutcome: "Optional output is present", PlannedAttempts: 1}},
	})
	candidate := fixtureCandidate(t, app, baseline)
	const close = "2026-11-17T11:59:00Z"
	optional := resultRow(candidate, "T3", "1", "OPTIONAL", "OPT-FAIL", close, close, "FAIL")
	optional[7], optional[9] = "BEH2", "POL2"
	importBytes(t, app, candidate.ID, csvBytes(t,
		resultRow(candidate, "T1", "1", "COMPLETE", "REQUIRED", "2026-11-17T11:45:00Z", close, "PASS"),
		resultRow(candidate, "T2", "1", "COMPLETE", "OLDER-RESULT", "2026-11-17T11:00:00Z", close, "PASS"),
		optional))
	atBoundary, err := app.Assess(supervisor, project, candidate.ID)
	if err != nil || atBoundary.Status != "SUFFICIENT" || !atBoundary.HasExpiry ||
		board.FormatUTC(atBoundary.EarliestExpiry) != "2026-11-17T12:00:00Z" || atBoundary.LedgerHighWater != 2 {
		t.Fatalf("exact expiration was not fresh from the oldest planned result: %s, %v", atBoundary.Status, err)
	}
	if atBoundary.Counts["PASS"] != 1 || atBoundary.Counts["FAIL"] != 0 ||
		len(atBoundary.Behaviors) != 2 || atBoundary.Behaviors[1].Status != "FAIL" || atBoundary.Behaviors[1].Required {
		t.Fatal("optional failure changed the required denominator or was hidden")
	}
	first, err := app.Freeze(supervisor, project, candidate.ID)
	if err != nil || first.Status != "SUFFICIENT" {
		t.Fatalf("exact expiry report was not sufficient: %s, %v", first.Status, err)
	}
	_, initialHTML, err := app.ReportHTML(supervisor, project, first.ID)
	if err != nil || !bytes.Contains(initialHTML, []byte("BEH2 · FAIL")) || !bytes.Contains(initialHTML, []byte("Optional · behavior revision 1")) {
		t.Fatalf("offline report lost optional evidence: %v", err)
	}
	now = now.Add(time.Microsecond)
	second, err := app.Freeze(supervisor, project, candidate.ID)
	if err != nil || second.Status != "BLOCKED" || second.ID == first.ID || second.LedgerHighWater != 2 {
		t.Fatalf("fresh export reused sufficient status after expiry: %s, %v", second.Status, err)
	}
	_, staleHTML, err := app.ReportHTML(supervisor, project, second.ID)
	if err != nil || !bytes.Contains(staleHTML, []byte("BEH1 · STALE")) || !bytes.Contains(staleHTML, []byte("STALE_CURRENT")) {
		t.Fatalf("offline expired report lost the blocking reason: %v", err)
	}
	_, preserved, err := app.ReportHTML(supervisor, project, first.ID)
	if err != nil || !bytes.Equal(initialHTML, preserved) {
		t.Fatalf("later assessment changed the earlier frozen bytes: %v", err)
	}
	history, err := app.Reports(supervisor, project, candidate.ID)
	if err != nil || !slices.EqualFunc(history, []board.Report{second, first}, func(a, b board.Report) bool { return a.ID == b.ID }) {
		t.Fatalf("history did not order snapshots by assessment time: %v", err)
	}
}
