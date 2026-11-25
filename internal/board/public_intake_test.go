package board_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestImportRejectsAmbiguousRunStructureAndPreservesPriorAssessment(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	baseline := vectorBaseline()
	baseline.Behaviors[0].Tests[0].PlannedAttempts = 2
	baseline.Behaviors = append(baseline.Behaviors, board.Behavior{
		ID: "BEH2", Revision: 1, Text: "Optional behavior",
		Policy: board.Policy{ID: "POL2", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
		Tests:  []board.TestPlan{{ID: "T2", ExpectedOutcome: "Observed optional data", PlannedAttempts: 1}},
	})
	candidate := fixtureCandidate(t, app, baseline)
	const earlier = "2026-11-17T11:00:00Z"
	const later = "2026-11-17T11:30:00Z"
	importBytes(t, app, candidate.ID, csvBytes(t,
		resultRow(candidate, "T1", "1", "Trusted", "Trusted1", earlier, earlier, "PASS"),
		resultRow(candidate, "T1", "2", "Trusted", "Trusted2", earlier, earlier, "PASS")))
	first := resultRow(candidate, "T1", "1", "Incoming", "Proposed1", later, later, "FAIL")
	otherInstant := resultRow(candidate, "T1", "2", "Incoming", "Proposed2", earlier, earlier, "PASS")
	otherBehavior := resultRow(candidate, "T2", "1", "Incoming", "Proposed2", later, later, "PASS")
	otherBehavior[7], otherBehavior[9] = "BEH2", "POL2"
	blind := runOnly(candidate, "Incoming", later)
	conflictingSlot := resultRow(candidate, "T1", "1", "Incoming", "Proposed2", later, later, "PASS")
	repeatResult := resultRow(candidate, "T1", "2", "Incoming", "Proposed1", later, later, "PASS")
	reuseResult := resultRow(candidate, "T1", "1", "Incoming", "Trusted1", later, later, "FAIL")
	for _, fixture := range []struct {
		name, code, column string
		rows               [][]string
		line               int
	}{
		{"same run spans behaviors", "RUN_IDENTITY", "run_id", [][]string{first, otherBehavior}, 3},
		{"same run has distinct finish instants", "RUN_FINISH_MISMATCH", "run_finished_at", [][]string{first, otherInstant}, 3},
		{"result precedes run-only row", "RUN_ONLY_MIXED", "record_type", [][]string{first, blind}, 3},
		{"run-only row precedes result", "RUN_ONLY_MIXED", "record_type", [][]string{blind, first}, 3},
		{"repeated run slot", "DUPLICATE_SLOT", "result_id", [][]string{first, conflictingSlot}, 3},
		{"repeated result across slots", "DUPLICATE_SLOT", "result_id", [][]string{first, repeatResult}, 3},
		{"result ID already committed", "RESULT_EXISTS", "result_id", [][]string{reuseResult}, 2},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			preview, err := app.PreviewImport(supervisor, project, candidate.ID, csvBytes(t, fixture.rows...))
			if err != nil || preview.Token != "" || len(preview.Issues) != 1 {
				t.Fatalf("invalid batch produced token=%t and %d issues, %v", preview.Token != "", len(preview.Issues), err)
			}
			issue := preview.Issues[0]
			if issue.Code != fixture.code || issue.Column != fixture.column || issue.PhysicalLine != fixture.line {
				t.Fatalf("wrong diagnostic code, column or line: %v", issue)
			}
			assessment, err := app.Assess(supervisor, project, candidate.ID)
			if err != nil || assessment.Status != board.Sufficient || assessment.LedgerHighWater != 1 || assessment.Behaviors[0].CurrentRunID != "Trusted" {
				t.Fatalf("rejected import altered prior evidence or assessment, %v", err)
			}
			switch fixture.name {
			case "same run has distinct finish instants":
				recordApprovalInstance(t, "AC-004-24.P003", map[string]any{"timingDefect": "conflicting run_finished_at in same run"}, "Reject the whole file with the offending timestamp or run identified")
			case "run-only row precedes result":
				recordApprovalInstance(t, "AC-004-26.P002", map[string]any{"invalidRun": "RESULT and RUN_ONLY in same file for same run"}, "Reject the whole file and keep earlier records intact")
			case "repeated run slot":
				recordApprovalInstance(t, "AC-005-07.P001", map[string]any{}, "Whole file rejected")
				recordApprovalInstance(t, "AC-005-09.P002", map[string]any{"duplicate": "same slot"}, "Whole file rejected")
			case "repeated result across slots":
				recordApprovalInstance(t, "AC-005-09.P001", map[string]any{"duplicate": "identical result_id"}, "Whole file rejected")
			case "result ID already committed":
				recordApprovalInstance(t, "AC-005-08.P001", map[string]any{}, "Whole file rejected")
			}
		})
		if t.Failed() {
			t.FailNow()
		}
	}
}

func TestImportChecksResultOrderingBeforeApplyingProducerClockAllowance(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	candidate := fixtureCandidate(t, app, vectorBaseline())
	const accepted = "2026-11-17T12:05:00.000000Z"
	const rejected = "2026-11-17T12:05:00.000001Z"
	for _, fixture := range []struct{ result, close, code, column string }{
		{rejected, accepted, "AFTER_RUN_FINISH", "finished_at"},
		{rejected, rejected, "CLOCK_SKEW_LIMIT", "run_finished_at"},
	} {
		preview, err := app.PreviewImport(supervisor, project, candidate.ID,
			csvBytes(t, resultRow(candidate, "T1", "1", "Incoming", "Observed", fixture.result, fixture.close, "PASS")))
		if err != nil || preview.Token != "" || len(preview.Issues) != 1 || preview.Issues[0].Code != fixture.code || preview.Issues[0].Column != fixture.column || preview.Issues[0].PhysicalLine != 2 {
			t.Fatalf("unexpected timestamp diagnostic: %#v, %v", preview.Issues, err)
		}
	}
	receipt := importBytes(t, app, candidate.ID,
		csvBytes(t, resultRow(candidate, "T1", "1", "Incoming", "Observed", accepted, accepted, "PASS")))
	if receipt.LedgerHighWater != 1 || !slices.Equal(receipt.AddedRuns, []string{"Incoming"}) {
		t.Fatal("inclusive clock allowance did not produce exactly one committed run")
	}
	recordApprovalInstance(t, "AC-004-24.P004", map[string]any{"timingDefect": "finished_at after run_finished_at"}, "Reject the whole file with the offending timestamp or run identified")
}

func TestImportRejectsRetiredRunIDsAfterAuthorizedRetention(t *testing.T) {
	now := fixtureTime
	app, _ := openBoard(t, func() time.Time { return now })
	prior := fixtureCandidate(t, app, vectorBaseline())
	stamp := board.FormatUTC(now)
	priorReceipt := importBytes(t, app, prior.ID, csvBytes(t, runOnly(prior, "RetiredRun", stamp)))
	now = now.Add(181 * 24 * time.Hour)
	scope := []string{prior.ID}
	plan, err := app.PreviewPurge(supervisor, project, scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Purge(supervisor, project, scope, plan.Confirmation); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SourceCSV(supervisor, project, priorReceipt.ID); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("purge retained original bytes: %v", err)
	}
	baseline := vectorBaseline()
	baseline.ID = "PostRetention"
	next := fixtureCandidate(t, app, baseline)
	preview, err := app.PreviewImport(supervisor, project, next.ID, csvBytes(t, runOnly(next, "RetiredRun", board.FormatUTC(now))))
	if err != nil || preview.Token != "" || len(preview.Issues) != 1 || preview.Issues[0].Code != "PURGED_RUN" || preview.Issues[0].Column != "run_id" {
		t.Fatalf("new candidate accepted retired run identifier: %#v, %v", preview.Issues, err)
	}
}
