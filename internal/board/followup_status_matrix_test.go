package board_test

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupEqualCompleteRunsUseLedgerOrdering(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	candidate := fixtureCandidate(t, app, vectorBaseline())
	receipt := importBytes(t, app, candidate.ID, csvBytes(t,
		resultRow(candidate, "T1", "1", "z", "z-result", intakeCurrent, intakeCurrent, "PASS"),
		resultRow(candidate, "T1", "1", "a", "a-result", intakeCurrent, intakeCurrent, "PASS")))
	assessment := statusAssessment(t, app, candidate.ID, "SUFFICIENT", 2)
	status := assessment.Behaviors[0]
	if receipt.LedgerHighWater != 2 || status.CurrentRunID != "z" ||
		statusRun(t, status, "a").Sequence != 1 || statusRun(t, status, "z").Sequence != 2 {
		t.Fatal("same-semantics timestamp tie did not follow the committed ledger")
	}
	recordCorroboratedBoardObservation(t)
}

func TestFollowupProducerCloseWinsBothUploadOrders(t *testing.T) {
	const resultAt = "2026-11-17T09:05:00Z"
	const completeAt = "2026-11-17T09:15:00Z"
	const laterClose = "2026-11-17T09:30:00Z"
	for _, zero := range []bool{false, true} {
		for _, laterFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("zero-%t/later-first-%t", zero, laterFirst), func(t *testing.T) {
				app, _ := openBoard(t, func() time.Time { return fixtureTime })
				roster := vectorBaseline()
				roster.Behaviors[0].Tests[0].PlannedAttempts = 2
				candidate := fixtureCandidate(t, app, roster)
				complete := csvBytes(t,
					resultRow(candidate, "T1", "1", "COMPLETE", "P-1", completeAt, completeAt, "PASS"),
					resultRow(candidate, "T1", "2", "COMPLETE", "P-2", completeAt, completeAt, "PASS"))
				incomplete := csvBytes(t, resultRow(candidate, "T1", "1", "LATER-CLOSE", "L-1", resultAt, laterClose, "PASS"))
				wantMissing := []int{2}
				if zero {
					incomplete = csvBytes(t, runOnly(candidate, "LATER-CLOSE", laterClose))
					wantMissing = []int{1, 2}
				}
				pairs := [][]byte{complete, incomplete}
				if laterFirst {
					pairs[0], pairs[1] = pairs[1], pairs[0]
				}
				for _, group := range pairs {
					importBytes(t, app, candidate.ID, group)
				}
				state := statusAssessment(t, app, candidate.ID, "BLOCKED", 2)
				behavior := state.Behaviors[0]
				if behavior.CurrentRunID != "LATER-CLOSE" || behavior.Status != "UNKNOWN" || len(behavior.Runs) != 2 {
					t.Fatal("earlier complete PASS displaced later incomplete current or removed history")
				}
				current := statusRun(t, behavior, "LATER-CLOSE")
				if current.Complete || current.Definitive || current.HasResults == zero ||
					board.FormatUTC(current.Closed) != laterClose || !statusRun(t, behavior, "COMPLETE").Complete {
					t.Fatal("one of the original independent run shapes was lost")
				}
				for _, attempt := range wantMissing {
					if !slices.ContainsFunc(behavior.Reasons, func(reason board.Reason) bool {
						return reason.Code == "MISSING_SLOT" && reason.TestID == "T1" && reason.Attempt == attempt
					}) {
						t.Fatalf("later close did not display missing planned attempt %d", attempt)
					}
				}
				t.Logf("later_close=%s zero_results=%t later_first=%t current=UNKNOWN candidate=BLOCKED gaps=%v", laterClose, zero, laterFirst, wantMissing)
				recordCorroboratedBoardObservation(t)
			})
		}
	}
}

func TestFollowupLaterCompleteRunDoesNotMergeOrEraseIncompleteHistory(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	roster := vectorBaseline()
	roster.Behaviors[0].Tests[0].PlannedAttempts = 3
	candidate := fixtureCandidate(t, app, roster)
	old := intakeThreeRows(candidate, "EARLIER-INCOMPLETE", intakeEarlier, "PASS")
	importBytes(t, app, candidate.ID, csvBytes(t, old[0]))
	fresh := intakeThreeRows(candidate, "LATER-COMPLETE", intakeCurrent, "PASS")
	importBytes(t, app, candidate.ID, csvBytes(t, fresh...))
	state := statusAssessment(t, app, candidate.ID, "SUFFICIENT", 2)
	behavior := state.Behaviors[0]
	historical := statusRun(t, behavior, "EARLIER-INCOMPLETE")
	if behavior.Status != "PASS" || behavior.CurrentRunID != "LATER-COMPLETE" || behavior.Passed != 3 ||
		len(behavior.Runs) != 2 || historical.Complete || historical.Definitive || len(historical.Rows) != 1 {
		t.Fatal("fresh complete evidence did not stay distinct from older incomplete history")
	}
	recordCorroboratedBoardObservation(t)
}

func TestFollowupCurrentRunCannotCrossCandidateOrBehavior(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	roster := twoRequiredBehaviors()
	candidate := fixtureCandidate(t, app, roster)
	manifest := vectorManifest()
	manifest.BuildDigest = strings.Repeat("b", 64)
	other, reused, err := app.CreateCandidate(supervisor, project, board.CandidateInput{
		Manifest: manifest, BaselineDigest: candidate.BaselineDigest,
	})
	if err != nil || reused || other.ID == candidate.ID {
		t.Fatalf("second candidate fixture did not preserve distinct execution identity: %v", err)
	}
	importBytes(t, app, candidate.ID, csvBytes(t, resultRow(candidate, "T1", "1", "TARGET", "TARGET-RESULT", intakeEarlier, intakeEarlier, "PASS")))
	foreign := resultRow(other, "T1", "1", "OTHER-CANDIDATE", "OTHER-C-RESULT", intakeMiddle, intakeMiddle, "FAIL")
	foreignReceipt := importBytes(t, app, other.ID, csvBytes(t, foreign))
	secondBehavior := resultRow(candidate, "T2", "1", "OTHER-BEHAVIOR", "OTHER-B-RESULT", intakeCurrent, intakeCurrent, "FAIL")
	secondBehavior[7], secondBehavior[9] = "BEH2", "POL2"
	behaviorReceipt := importBytes(t, app, candidate.ID, csvBytes(t, secondBehavior))
	target := statusAssessment(t, app, candidate.ID, "BLOCKED", 3)
	if foreignReceipt.LedgerHighWater != 2 || behaviorReceipt.LedgerHighWater != 3 ||
		len(target.Behaviors) != 2 || target.Behaviors[0].ID != "BEH1" || target.Behaviors[0].CurrentRunID != "TARGET" ||
		target.Behaviors[0].Status != "PASS" || len(target.Behaviors[0].Runs) != 1 ||
		target.Behaviors[1].ID != "BEH2" || target.Behaviors[1].CurrentRunID != "OTHER-BEHAVIOR" ||
		target.Behaviors[1].Status != "FAIL" {
		t.Fatal("larger foreign candidate or behavior sequence interfered with exact target selection")
	}
	recordCorroboratedBoardObservation(t)
}

func TestFollowupExactThresholdsAndImmediateMustPassFailure(t *testing.T) {
	for _, check := range []struct {
		name      string
		mark      int
		mandatory string
		optional  string
		omit      bool
		status    string
		summary   string
		passed    int
		reasons   []string
	}{
		{"three-of-four-at-cut", 7500, "PASS", "FAIL", false, "PASS", "SUFFICIENT", 3, nil},
		{"three-of-four-one-bps-over", 7501, "PASS", "FAIL", false, "FAIL", "BLOCKED", 3, []string{"RATE_BELOW_THRESHOLD"}},
		{"zero-threshold-mandatory-fail", 0, "FAIL", "PASS", false, "FAIL", "BLOCKED", 3, []string{"MUST_PASS_FAILED"}},
		{"must-pass-fail-and-missing", 7500, "FAIL", "PASS", true, "FAIL", "BLOCKED", 2, []string{"MUST_PASS_FAILED", "MISSING_SLOT"}},
	} {
		t.Run(check.name, func(t *testing.T) {
			app, _ := openBoard(t, func() time.Time { return fixtureTime })
			roster := vectorBaseline()
			roster.Behaviors[0].Policy.MinPassBasisPoints = check.mark
			roster.Behaviors[0].Tests = append(roster.Behaviors[0].Tests, board.TestPlan{
				ID: "T2", ExpectedOutcome: "Three independent non-mandatory observations", PlannedAttempts: 3,
			})
			candidate := fixtureCandidate(t, app, roster)
			makeRow := func(test, attempt, result, outcome string) []string {
				return resultRow(candidate, test, attempt, "RATE", result, intakeCurrent, intakeCurrent, outcome)
			}
			rows := [][]string{
				makeRow("T1", "1", "MANDATORY", check.mandatory),
				makeRow("T2", "1", "OPTIONAL-1", check.optional),
				makeRow("T2", "2", "OPTIONAL-2", "PASS"),
				makeRow("T2", "3", "OPTIONAL-3", "PASS"),
			}
			if check.omit {
				rows = rows[:3]
			}
			importBytes(t, app, candidate.ID, csvBytes(t, rows...))
			decision := statusAssessment(t, app, candidate.ID, check.summary, 1)
			behavior := decision.Behaviors[0]
			if behavior.Status != check.status || behavior.Planned != 4 || behavior.Passed != check.passed {
				t.Fatalf("wrong exact planned denominator or threshold outcome: %s %d/%d", behavior.Status, behavior.Passed, behavior.Planned)
			}
			requireBehaviorReasons(t, behavior.Reasons, check.reasons...)
			recordCorroboratedBoardObservation(t)
		})
	}
}

func TestFollowupIndependentUnreachableDenominatorGapsPersist(t *testing.T) {
	for _, unusable := range []bool{false, true} {
		t.Run(fmt.Sprintf("unusable-%t", unusable), func(t *testing.T) {
			app, _ := openBoard(t, func() time.Time { return fixtureTime })
			roster := vectorBaseline()
			required := &roster.Behaviors[0]
			required.Policy.MinPassBasisPoints = 8000
			required.Tests[0].PlannedAttempts = 5
			required.Tests = append(required.Tests, board.TestPlan{ID: "T2", ExpectedOutcome: "Independent slots", PlannedAttempts: 5})
			candidate := fixtureCandidate(t, app, roster)
			var earlier, later [][]string
			for attempt := 1; attempt <= 5; attempt++ {
				index := strconv.Itoa(attempt)
				earlier = append(earlier, resultRow(candidate, "T1", index, "UNREACHABLE", "A-T1-"+index, intakeEarlier, intakeEarlier, "PASS"))
				for _, test := range []string{"T1", "T2"} {
					later = append(later, resultRow(candidate, test, index, "FRESH-PASS", "B-"+test+"-"+index, intakeCurrent, intakeCurrent, "PASS"))
				}
			}
			for attempt := 1; attempt <= 5; attempt++ {
				index := strconv.Itoa(attempt)
				outcome := "FAIL"
				if attempt > 3 {
					if !unusable {
						continue
					}
					outcome = "ERROR"
				}
				earlier = append(earlier, resultRow(candidate, "T2", index, "UNREACHABLE", "A-T2-"+index, intakeEarlier, intakeEarlier, outcome))
			}
			importBytes(t, app, candidate.ID, csvBytes(t, earlier...))
			importBytes(t, app, candidate.ID, csvBytes(t, later...))
			decision := statusAssessment(t, app, candidate.ID, "BLOCKED", 2)
			behavior := decision.Behaviors[0]
			prior := statusRun(t, behavior, "UNREACHABLE")
			if behavior.Status != "FAIL" || behavior.CurrentRunID != "FRESH-PASS" || behavior.Passed != 10 ||
				len(behavior.Runs) != 2 || !prior.Definitive || prior.BestPossible != 7 || prior.Planned != 10 || prior.Failed != 3 {
				t.Fatal("the later 10/10 pass erased an unreachable historical threshold")
			}
			gap := "MISSING_SLOT"
			if unusable {
				gap = "ERROR_RESULT"
			}
			requireBehaviorReasons(t, prior.Reasons, "RATE_BELOW_THRESHOLD", gap)
			recordCorroboratedBoardObservation(t)
		})
	}
}

func TestFollowupEachBlockingStatusKeepsItsOwnRequiredCount(t *testing.T) {
	for _, status := range []string{"FAIL", "UNKNOWN", "STALE"} {
		t.Run(status, func(t *testing.T) {
			app, _ := openBoard(t, func() time.Time { return fixtureTime })
			roster := vectorBaseline()
			roster.Behaviors[0].Policy.MaxAgeHours = 1
			candidate := fixtureCandidate(t, app, roster)
			var row []string
			switch status {
			case "FAIL":
				row = resultRow(candidate, "T1", "1", "COUNTED", "COUNT-FAIL", intakeCurrent, intakeCurrent, "FAIL")
			case "UNKNOWN":
				row = runOnly(candidate, "COUNTED", intakeCurrent)
			case "STALE":
				row = resultRow(candidate, "T1", "1", "COUNTED", "COUNT-STALE", intakeEarlier, intakeEarlier, "PASS")
			}
			importBytes(t, app, candidate.ID, csvBytes(t, row))
			decision := statusAssessment(t, app, candidate.ID, "BLOCKED", 1)
			if decision.Behaviors[0].Status != status || decision.Counts[status] != 1 {
				t.Fatal("required blocker moved to a different summary count")
			}
			for _, other := range []string{"PASS", "FAIL", "UNKNOWN", "STALE"} {
				if other != status && decision.Counts[other] != 0 {
					t.Fatalf("unrelated status %s has a nonzero required count", other)
				}
			}
			recordCorroboratedBoardObservation(t)
		})
	}
}

func TestFollowupNoEvidenceCountsEveryApprovedRequiredBehavior(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	candidate := fixtureCandidate(t, app, twoRequiredBehaviors())
	decision := statusAssessment(t, app, candidate.ID, "BLOCKED", 0)
	if len(decision.Behaviors) != 2 || decision.Counts["UNKNOWN"] != 2 ||
		decision.Counts["PASS"] != 0 || decision.Counts["FAIL"] != 0 || decision.Counts["STALE"] != 0 {
		t.Fatal("approved required roster was not counted without imports")
	}
	for _, behavior := range decision.Behaviors {
		if !behavior.Required || behavior.Status != "UNKNOWN" || behavior.CurrentRunID != "" {
			t.Fatal("missing evidence produced a current run or assessed claim")
		}
		requireBehaviorReasons(t, behavior.Reasons, "MISSING_RUN")
	}
	recordCorroboratedBoardObservation(t)
}

func twoRequiredBehaviors() board.Baseline {
	roster := vectorBaseline()
	roster.Behaviors = append(roster.Behaviors, board.Behavior{
		ID: "BEH2", Required: true, Revision: 1, Text: "Independent second behavior",
		Policy: board.Policy{ID: "POL2", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
		Tests:  []board.TestPlan{{ID: "T2", ExpectedOutcome: "Second behavior observed", MustPass: true, PlannedAttempts: 1}},
	})
	return roster
}

func statusAssessment(t *testing.T, app *board.Board, candidate, want string, ledger uint64) board.Assessment {
	t.Helper()
	current, err := app.Assess(supervisor, project, candidate)
	if err != nil || current.Status != want || current.LedgerHighWater != ledger || len(current.Behaviors) == 0 {
		t.Fatalf("assessment status=%q ledger=%d error=%v; want %s at %d", current.Status, current.LedgerHighWater, err, want, ledger)
	}
	return current
}

func statusRun(t *testing.T, behavior board.BehaviorAssessment, id string) board.RunAssessment {
	t.Helper()
	index := slices.IndexFunc(behavior.Runs, func(run board.RunAssessment) bool { return run.ID == id })
	if index < 0 {
		t.Fatalf("assessment has no retained run %s", id)
	}
	return behavior.Runs[index]
}

func requireBehaviorReasons(t *testing.T, actual []board.Reason, codes ...string) {
	t.Helper()
	for _, code := range codes {
		if !slices.ContainsFunc(actual, func(reason board.Reason) bool { return reason.Code == code }) {
			t.Errorf("assessment omitted %s", code)
		}
	}
}
