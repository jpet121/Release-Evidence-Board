package board_test

import (
	"bytes"
	"encoding/csv"
	"slices"
	"testing"

	"release-evidence-board/internal/board"
)

// The parameter labels name only individually observed predicates. The
// independent scenario has already run; inferring acceptance from its mere
// presence in the historical corpus would admit cases it did not observe.
type oracleAcceptanceRow struct {
	id         string
	parameters map[string]any
	result     string
}

var scopedOracleParameters = map[string][]oracleAcceptanceRow{
	"IOR-001": {
		{"AC-005-03.P001", map[string]any{"concurrentExactRetries": 2}, "Original receipt returned; no new sequence"},
	},
	"IOR-004": {
		{"AC-004-25.P001", map[string]any{}, "One run is stored with zero results. List every planned slot as missing"},
		{"AC-006-11.P002", map[string]any{"incompleteness": "RUN_ONLY", "uploadOrder": []string{"incomplete", "passing"}}, "The incomplete run stays current and UNKNOWN in each order. The candidate is BLOCKED, the applicable evidence reasons are visible, and both runs stay in history"},
	},
	"IOR-005": {
		{"AC-006-11.P001", map[string]any{"incompleteness": "RUN_ONLY", "uploadOrder": []string{"passing", "incomplete"}}, "The incomplete run stays current and UNKNOWN in each order. The candidate is BLOCKED, the applicable evidence reasons are visible, and both runs stay in history"},
	},
	"IOR-006": {
		{"AC-004-02.P001", map[string]any{}, "Stored; missing slots listed; current behavior UNKNOWN absent definitive failure"},
		{"AC-006-11.P004", map[string]any{"incompleteness": "missing slots", "uploadOrder": []string{"incomplete", "passing"}}, "The incomplete run stays current and UNKNOWN in each order. The candidate is BLOCKED, the applicable evidence reasons are visible, and both runs stay in history"},
	},
	"IOR-007": {
		{"AC-006-11.P003", map[string]any{"incompleteness": "missing slots", "uploadOrder": []string{"passing", "incomplete"}}, "The incomplete run stays current and UNKNOWN in each order. The candidate is BLOCKED, the applicable evidence reasons are visible, and both runs stay in history"},
	},
	"IOR-008": {
		{"AC-006-11.P006", map[string]any{"incompleteness": "ERROR", "uploadOrder": []string{"incomplete", "passing"}}, "The incomplete run stays current and UNKNOWN in each order. The candidate is BLOCKED, the applicable evidence reasons are visible, and both runs stay in history"},
	},
	"IOR-009": {
		{"AC-006-11.P005", map[string]any{"incompleteness": "ERROR", "uploadOrder": []string{"passing", "incomplete"}}, "The incomplete run stays current and UNKNOWN in each order. The candidate is BLOCKED, the applicable evidence reasons are visible, and both runs stay in history"},
	},
	"IOR-010": {
		{"AC-006-11.P008", map[string]any{"incompleteness": "SKIPPED", "uploadOrder": []string{"incomplete", "passing"}}, "The incomplete run stays current and UNKNOWN in each order. The candidate is BLOCKED, the applicable evidence reasons are visible, and both runs stay in history"},
	},
	"IOR-011": {
		{"AC-006-11.P007", map[string]any{"incompleteness": "SKIPPED", "uploadOrder": []string{"passing", "incomplete"}}, "The incomplete run stays current and UNKNOWN in each order. The candidate is BLOCKED, the applicable evidence reasons are visible, and both runs stay in history"},
	},
	"IOR-012": {
		{"AC-006-11.P010", map[string]any{"incompleteness": "UNKNOWN", "uploadOrder": []string{"incomplete", "passing"}}, "The incomplete run stays current and UNKNOWN in each order. The candidate is BLOCKED, the applicable evidence reasons are visible, and both runs stay in history"},
	},
	"IOR-013": {
		{"AC-006-11.P009", map[string]any{"incompleteness": "UNKNOWN", "uploadOrder": []string{"passing", "incomplete"}}, "The incomplete run stays current and UNKNOWN in each order. The candidate is BLOCKED, the applicable evidence reasons are visible, and both runs stay in history"},
	},
	"IOR-015": {
		{"AC-006-05.P001", map[string]any{}, "No stitching; current incomplete run stays UNKNOWN"},
		{"AC-007-07.P001", map[string]any{}, "UNKNOWN with MISSING_SLOT"},
	},
	"IOR-016": {
		{"AC-006-07.P001", map[string]any{}, "FAIL persists; both runs visible"},
		{"AC-007-19.P001", map[string]any{"cooccurringEvidence": "missing slot"}, "Primary FAIL and every applicable reason shown"},
		{"AC-007-19.P002", map[string]any{"cooccurringEvidence": "ERROR"}, "Primary FAIL and every applicable reason shown"},
		{"AC-007-22.P003", map[string]any{"plannedAttempts": 10, "requiredPasses": 8, "nonMustPassFailures": 3, "remainingEvidenceGap": "both missing and unusable"}, "FAIL persists. The earlier run reports RATE_BELOW_THRESHOLD with a best possible score of seven out of ten. The missing or unusable evidence and both runs remain visible"},
	},
	"IOR-017": {
		{"AC-007-04.P001", map[string]any{"bestPossibleRateVsThreshold": "equal"}, "UNKNOWN with missing or unusable slots visible. The rate is still attainable"},
	},
	"IOR-018": {
		{"AC-007-04.P002", map[string]any{"bestPossibleRateVsThreshold": "above"}, "UNKNOWN with missing or unusable slots visible. The rate is still attainable"},
	},
	"IOR-019": {
		{"AC-007-21.P001", map[string]any{"t1Attempts": 9, "t1Passes": 9, "t2Attempts": 1, "t2Passes": 0, "thresholdBasisPoints": 9000, "t2MustPass": false}, "All variants have a 90% observed pass rate. The 9000 non-must-pass variant passes. The 9001 variant fails the rate threshold. The must-pass variant fails because its required attempt failed"},
	},
	"IOR-020": {
		{"AC-007-21.P002", map[string]any{"t1Attempts": 9, "t1Passes": 9, "t2Attempts": 1, "t2Passes": 0, "thresholdBasisPoints": 9001, "t2MustPass": false}, "All variants have a 90% observed pass rate. The 9000 non-must-pass variant passes. The 9001 variant fails the rate threshold. The must-pass variant fails because its required attempt failed"},
	},
	"IOR-021": {
		{"AC-007-21.P003", map[string]any{"t1Attempts": 9, "t1Passes": 9, "t2Attempts": 1, "t2Passes": 0, "thresholdBasisPoints": 9000, "t2MustPass": true}, "All variants have a 90% observed pass rate. The 9000 non-must-pass variant passes. The 9001 variant fails the rate threshold. The must-pass variant fails because its required attempt failed"},
	},
	"IOR-022": {
		{"AC-007-15.P001", map[string]any{"runCloseOffsetMicroseconds": 300000000, "finishedAt": "equal to run close"}, "Accepted with CLOCK_SKEW. Age derives from the oldest result and clamps to zero for a future result"},
	},
	"IOR-023": {
		{"AC-007-15.P002", map[string]any{"runCloseOffsetMicroseconds": 300000000, "finishedAt": "in past"}, "Accepted with CLOCK_SKEW. Age derives from the oldest result and clamps to zero for a future result"},
	},
	"IOR-024": {
		{"AC-007-16.P001", map[string]any{"runCloseOffsetMicroseconds": 300000001, "finishedAt": "equal to run close"}, "Whole file rejected"},
	},
	"IOR-025": {
		{"AC-007-16.P002", map[string]any{"runCloseOffsetMicroseconds": 300000001, "finishedAt": "in past"}, "Whole file rejected"},
	},
	"IOR-026": {
		{"AC-007-13.P001", map[string]any{"microsecondsPastFreshnessLimit": 0}, "PASS"},
		{"AC-007-14.P001", map[string]any{"microsecondsPastFreshnessLimit": 1}, "STALE"},
		{"AC-007-17.P001", map[string]any{}, "Oldest slot controls freshness"},
		{"AC-010-04.P001", map[string]any{}, "Fresh calculation shows STALE, not cached PASS"},
		{"AC-010-06.P001", map[string]any{}, "Historical assessment time and expiry prominent; no current-pass claim"},
	},
	"IOR-027": {
		{"AC-002-19.P001", map[string]any{}, "Approval records the old and new marks, reducing actors, approving reviewer and time. The old failure remains. A successor starts with its own missing evidence"},
		{"AC-007-03.P001", map[string]any{}, "FAIL persists; stale evidence never clears failure"},
		{"AC-007-19.P003", map[string]any{"cooccurringEvidence": "stale current complete run"}, "Primary FAIL and every applicable reason shown"},
	},
	"IOR-029": {
		{"AC-003-06.P001", map[string]any{}, "Existing candidate returned; no duplicate"},
	},
	"IOR-030": {
		{"AC-003-09.P001", map[string]any{}, "Predecessor link recorded automatically"},
		{"AC-003-14.P001", map[string]any{"observation": "before new evidence"}, "Predecessor and changed input are visible; successor is BLOCKED with UNKNOWN required behaviors before new evidence and SUFFICIENT after it; predecessor stays BLOCKED"},
		{"AC-003-14.P002", map[string]any{"observation": "after fresh sufficient evidence"}, "Predecessor and changed input are visible; successor is BLOCKED with UNKNOWN required behaviors before new evidence and SUFFICIENT after it; predecessor stays BLOCKED"},
		{"AC-004-05.P001", map[string]any{}, "Warning; stored against historical pair only"},
		{"AC-007-06.P001", map[string]any{}, "UNKNOWN with MISSING_RUN"},
		{"AC-008-06.P001", map[string]any{}, "No carryover; missing evidence UNKNOWN"},
	},
	"IOR-031": {
		{"AC-003-01.P001", map[string]any{"vector": "V01"}, "Exact bytes and digests match V01 and V02"},
		{"AC-003-01.P002", map[string]any{"vector": "V02"}, "Exact bytes and digests match V01 and V02"},
		{"AC-003-03.P001", map[string]any{"pairedInputs": "NFC é versus decomposed e + U+0301"}, "Distinct bytes and digests; no silent normalization"},
		{"AC-003-03.P002", map[string]any{"pairedInputs": "one space versus two spaces"}, "Distinct bytes and digests; no silent normalization"},
		{"AC-003-10.P001", map[string]any{"character": "double quote"}, "Exact C01 escaping, UTF-8, no BOM/newline"},
		{"AC-003-10.P002", map[string]any{"character": "backslash"}, "Exact C01 escaping, UTF-8, no BOM/newline"},
		{"AC-003-10.P004", map[string]any{"character": "U+000A"}, "Exact C01 escaping, UTF-8, no BOM/newline"},
		{"AC-003-10.P005", map[string]any{"character": "é"}, "Exact C01 escaping, UTF-8, no BOM/newline"},
		{"AC-003-10.P006", map[string]any{"character": "U+2028"}, "Exact C01 escaping, UTF-8, no BOM/newline"},
	},
	"IOR-033": {
		{"AC-004-01.P001", map[string]any{}, "Parsed values and linked candidate match fixture"},
		{"AC-010-07.P001", map[string]any{"evidenceVector": "V03"}, "Matches V03; HTML hash stored separately"},
	},
	"IOR-034": {
		{"AC-010-07.P002", map[string]any{"evidenceVector": "V07"}, "Matches V03; HTML hash stored separately"},
	},
	"IOR-035": {
		{"AC-010-07.P003", map[string]any{"evidenceVector": "V08"}, "Matches V03; HTML hash stored separately"},
	},
	"IOR-036": {
		{"AC-010-10.P001", map[string]any{"requiredContent": "incomplete including tied pass", "fixture": "single frozen mixed-status report"}, "Finish times, slot completeness, ledger sequences, policy, result timestamps and outcomes reproduce the reported statuses at the recorded assessment time"},
		{"AC-010-10.P002", map[string]any{"requiredContent": "stale", "fixture": "single frozen mixed-status report"}, "Finish times, slot completeness, ledger sequences, policy, result timestamps and outcomes reproduce the reported statuses at the recorded assessment time"},
		{"AC-010-10.P003", map[string]any{"requiredContent": "passing", "fixture": "single frozen mixed-status report"}, "Finish times, slot completeness, ledger sequences, policy, result timestamps and outcomes reproduce the reported statuses at the recorded assessment time"},
		{"AC-010-10.P004", map[string]any{"requiredContent": "persistently failing", "fixture": "single frozen mixed-status report"}, "Finish times, slot completeness, ledger sequences, policy, result timestamps and outcomes reproduce the reported statuses at the recorded assessment time"},
	},
	"IOR-037": {
		{"AC-004-26.P003", map[string]any{"invalidRun": "RESULT follows a previously committed RUN_ONLY in a later upload"}, "Reject the whole file and keep earlier records intact"},
		{"AC-005-12.P001", map[string]any{"retry": "same original bytes"}, "Return the original or no-op receipt respectively. Create no new run, result or ledger sequence"},
		{"AC-005-12.P002", map[string]any{"retry": "same parsed record with changed quoting"}, "Return the original or no-op receipt respectively. Create no new run, result or ledger sequence"},
	},
	"IOR-040": {
		{"AC-004-10.P001", map[string]any{"syntaxDefect": "invalid UTF-8"}, "Whole file rejected with logical row/physical line and column where identifiable"},
	},
	"IOR-042": {
		{"AC-007-20.P001", map[string]any{}, "FAIL calculated; exclusion from required summary handled by REQ-008"},
		{"AC-008-01.P001", map[string]any{}, "SUFFICIENT"},
		{"AC-008-05.P001", map[string]any{}, "SUFFICIENT; optional failure remains visible"},
	},
	"IOR-045": {
		{"AC-010-09.P001", map[string]any{}, "Expiry marked none; status remains blocked"},
	},
	"IOR-053": {
		{"AC-006-03.P001", map[string]any{}, "Preview shows the earlier run in history, the later-finished run still current, and the evidence gap still visible"},
	},
}

func (e *independentEnvironment) recordScopedOracleAcceptance() {
	rows := scopedOracleParameters[e.scenario.ID]
	if len(rows) == 0 {
		return
	}
	e.verifyScopedOracleObservations()
	if e.t.Failed() {
		return
	}
	for _, row := range rows {
		recordApprovalInstance(e.t, row.id, row.parameters, row.result)
	}
}

func (e *independentEnvironment) verifyScopedOracleObservations() {
	t := e.t
	t.Helper()
	switch e.scenario.ID {
	case "IOR-004", "IOR-005", "IOR-006", "IOR-007", "IOR-008", "IOR-009", "IOR-010", "IOR-011", "IOR-012", "IOR-013":
		e.verifyScopedTiedRunHistory()
	case "IOR-015", "IOR-016":
		result, err := e.b.Assess(independentOwner, independentProject, e.candidate.ID)
		independentRequire(t, err)
		behavior := independentBehavior(t, result, "BEH1")
		if len(behavior.Runs) != 2 {
			t.Fatal("earlier snapshot disappeared after the separate rerun")
		}
		if e.scenario.ID == "IOR-016" {
			prior, current := observedOracleRun(t, behavior, "partial"), observedOracleRun(t, behavior, "full")
			if !prior.Definitive || prior.BestPossible != 7 || prior.Failed != 3 || prior.Planned != 10 || !current.Complete || current.Passed != 10 || behavior.Policy.MinPassBasisPoints != 8000 {
				t.Fatal("history lost the exact unreachable original mark and completed positive rerun")
			}
		} else {
			a, b := observedOracleRun(t, behavior, "a"), observedOracleRun(t, behavior, "b")
			if a.Complete || b.Complete || len(a.Rows) != 1 || len(b.Rows) != 1 || a.Rows[0].TestID() == b.Rows[0].TestID() {
				t.Fatal("retained snapshots are not distinct complementary missing-slot runs")
			}
		}
	case "IOR-019", "IOR-020", "IOR-021":
		result, err := e.b.Assess(independentOwner, independentProject, e.candidate.ID)
		independentRequire(t, err)
		behavior := independentBehavior(t, result, "BEH1")
		mustPass := e.scenario.ID == "IOR-021"
		mark := 9000
		if e.scenario.ID == "IOR-020" {
			mark = 9001
		}
		if len(behavior.Tests) != 2 || behavior.Tests[0].ID != "T1" || behavior.Tests[0].PlannedAttempts != 9 || behavior.Tests[1].ID != "T2" || behavior.Tests[1].PlannedAttempts != 1 || behavior.Tests[1].MustPass != mustPass || behavior.Policy.MinPassBasisPoints != mark {
			t.Fatal("approved weighted-rate inputs do not match this exact fixture")
		}
		observed := observedOracleRun(t, behavior, "weighted")
		failed := 0
		for _, row := range observed.Rows {
			switch {
			case row.TestID() == "T1" && row.Outcome() == "PASS":
			case row.TestID() == "T2" && row.Outcome() == "FAIL":
				failed++
			default:
				t.Fatal("weighted-rate result slots were not recorded as submitted")
			}
		}
		if len(observed.Rows) != 10 || failed != 1 {
			t.Fatal("weighted-rate fixture did not retain precisely 9 PASS and one T2 FAIL")
		}
	case "IOR-022", "IOR-023":
		result, err := e.b.Assess(independentOwner, independentProject, e.candidate.ID)
		independentRequire(t, err)
		behavior := independentBehavior(t, result, "BEH1")
		want := int64(0)
		if e.scenario.ID == "IOR-023" {
			want = 3600000000
		}
		if behavior.AgeMicroseconds == nil || *behavior.AgeMicroseconds != want {
			t.Fatalf("age did not come from the oldest literal result, expected %d microseconds", want)
		}
	case "IOR-033":
		candidate, _, _, err := e.b.Candidate(independentOwner, independentProject, e.candidate.ID)
		independentRequire(t, err)
		result, err := e.b.Assess(independentOwner, independentProject, candidate.ID)
		independentRequire(t, err)
		behavior := independentBehavior(t, result, "BEH1")
		observed := observedOracleRun(t, behavior, "RUN1")
		parsed, err := csv.NewReader(bytes.NewReader(e.raw("RUN1"))).ReadAll()
		independentRequire(t, err)
		if len(parsed) != 2 || len(observed.Rows) != 1 || !slices.Equal(observed.Rows[0].Fields[:], parsed[1]) || observed.CandidateID != candidate.ID || observed.ReceiptID != e.receipts["RUN1"].ID || observed.Rows[0].LogicalRow != 2 {
			t.Fatal("committed Appendix V CSV fields or authenticated candidate identity changed")
		}
	}
}

func (e *independentEnvironment) verifyScopedTiedRunHistory() {
	t := e.t
	t.Helper()
	assessed, err := e.b.Assess(independentOwner, independentProject, e.candidate.ID)
	independentRequire(t, err)
	behavior := independentBehavior(t, assessed, "BEH1")
	if len(behavior.Runs) != 2 || behavior.CurrentRunID != "incomplete" || !behavior.CurrentTieIncomplete {
		t.Fatal("tied run history did not preserve both snapshots and the incomplete winner")
	}
	incomplete := observedOracleRun(t, behavior, "incomplete")
	passing := observedOracleRun(t, behavior, "passing")
	if incomplete.Complete || incomplete.Definitive || !passing.Complete || !passing.Closed.Equal(incomplete.Closed) {
		t.Fatal("passing and incomplete snapshots were not both retained at their actual tied close")
	}
	historical := independentDecode[struct{ History []string }](t, e.scenario.Expected).History
	if len(historical) != 2 || observedOracleRun(t, behavior, historical[0]).Sequence != 1 || observedOracleRun(t, behavior, historical[1]).Sequence != 2 {
		t.Fatal("producer tied-run upload and ledger order did not match the independent fixture")
	}
	switch e.scenario.ID {
	case "IOR-004", "IOR-005":
		if len(incomplete.Rows) != 1 || incomplete.Rows[0].Kind() != "RUN_ONLY" || incomplete.Rows[0].ResultID() != "" || incomplete.Passed != 0 {
			t.Fatal("stored run-only observation gained a result slot")
		}
	case "IOR-006":
		if len(incomplete.Rows) != 1 || incomplete.Rows[0].Kind() != "RESULT" || incomplete.Rows[0].TestID() != "T1" {
			t.Fatal("stored partial observation did not retain its first slot")
		}
	}
}

func observedOracleRun(t *testing.T, behavior board.BehaviorAssessment, id string) board.RunAssessment {
	t.Helper()
	for _, run := range behavior.Runs {
		if run.ID == id {
			return run
		}
	}
	t.Fatalf("expected run %s is absent from retained history", id)
	return board.RunAssessment{}
}
