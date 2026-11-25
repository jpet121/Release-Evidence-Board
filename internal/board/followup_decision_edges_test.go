package board_test

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupCSVBoundaryDiagnosticsPreserveCommittedEvidence(t *testing.T) {
	f := newAtomicIntake(t)
	row := resultRow(f.candidate, "T1", "1", "VALID-PREFIX", "PREFIX-RESULT", intakeCurrent, intakeCurrent, "PASS")
	valid := string(csvBytes(t, row))
	line := strings.TrimSuffix(strings.TrimPrefix(valid, resultsHeader), "\n")
	for _, input := range []struct {
		name, raw, column string
		line              int
	}{
		{"malformed_header_quote", `"unclosed`, "schema_version", 1},
		{"unclosed_extra_field", valid + line + `,"extra`, "", 3},
		{"suffix_after_extra_field", valid + line + `,"extra"x`, "", 3},
		{"terminal_cr", valid + line + "\r", "", 3},
	} {
		t.Run(input.name, func(t *testing.T) {
			f.rejectRaw(t, []byte(input.raw), "CSV_SYNTAX", input.line, input.column)
		})
	}
	for _, input := range []struct{ name, uri string }{
		{"literal_backslash", `https://example.invalid/dir\file`},
		{"bad_percent_escape", "https://example.invalid/%zz"},
		{"authority_missing", "https:opaque"},
	} {
		t.Run(input.name, func(t *testing.T) {
			copyRow := slices.Clone(row)
			copyRow[17] = input.uri
			f.rejectRaw(t, csvBytes(t, copyRow), "SOURCE_URI", 2, "source_uri")
		})
	}

	eof := resultRow(f.candidate, "T1", "1", "QUOTED-EOF", "EOF-RESULT", intakeMiddle, intakeMiddle, "PASS")
	eof[18] = `a final "quoted" field, no trailing record separator`
	raw := bytes.TrimSuffix(csvBytes(t, eof), []byte("\n"))
	receipt := importBytes(t, f.app, f.candidate.ID, raw)
	if receipt.LedgerHighWater != 3 || receipt.Rows != 1 {
		t.Fatal("a well-formed quoted field at EOF did not commit exactly one new row")
	}
	checkImportedCSVRow(t, f.app, f.candidate.ID, receipt.ID, raw, eof)
}

func TestFollowupOldestResultAfterStableSlotOrdering(t *testing.T) {
	for _, oldest := range []string{"first_slot", "second_slot"} {
		t.Run(oldest, func(t *testing.T) {
			now := fixtureTime
			app, _ := openBoard(t, func() time.Time { return now })
			roster := vectorBaseline()
			roster.Behaviors[0].Tests[0].PlannedAttempts = 2
			candidate := fixtureCandidate(t, app, roster)
			first, second := intakeMiddle, intakeCurrent
			if oldest == "second_slot" {
				first, second = second, first
			}
			receipt := importBytes(t, app, candidate.ID, csvBytes(t,
				resultRow(candidate, "T1", "2", "SLOTS", "SLOT-2", second, intakeCurrent, "PASS"),
				resultRow(candidate, "T1", "1", "SLOTS", "SLOT-1", first, intakeCurrent, "PASS")))
			initial := statusAssessment(t, app, candidate.ID, "SUFFICIENT", 1)
			const expiry = "2026-11-18T10:30:00Z"
			if receipt.Rows != 2 || !initial.HasExpiry || board.FormatUTC(initial.EarliestExpiry) != expiry ||
				initial.Behaviors[0].Passed != 2 || initial.Behaviors[0].Planned != 2 {
				t.Errorf("oldest attempt did not set the independent expected expiry and denominator: %v", initial.EarliestExpiry)
			}
			now = time.Date(2026, 11, 18, 10, 30, 0, 0, time.UTC)
			statusAssessment(t, app, candidate.ID, "SUFFICIENT", 1)
			now = now.Add(time.Microsecond)
			stale := statusAssessment(t, app, candidate.ID, "BLOCKED", 1)
			behavior := stale.Behaviors[0]
			if behavior.Status != "STALE" || behavior.CurrentRunID != "SLOTS" ||
				!slices.ContainsFunc(behavior.Reasons, func(reason board.Reason) bool {
					return reason.Code == "STALE_CURRENT" && reason.RunID == "SLOTS"
				}) {
				t.Error("current run did not become stale one microsecond after the oldest result's expiry")
			}
		})
	}
}

func TestFollowupDefinitiveFailureSurvivesRetryAndRemainsSeparateFromSuccessor(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	first := fixtureCandidate(t, app, vectorBaseline())
	failCSV := csvBytes(t, resultRow(first, "T1", "1", "EARLIER-FAIL", "FAILED-RESULT", intakeEarlier, intakeEarlier, "FAIL"))
	staged := previewFollowupFile(t, app, first.ID, failCSV)
	original, err := app.CommitImport(supervisor, project, staged.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	importBytes(t, app, first.ID, csvBytes(t, resultRow(first, "T1", "1", "NEWER-PASS", "PASSED-RESULT", intakeCurrent, intakeCurrent, "PASS")))
	held := statusAssessment(t, app, first.ID, "BLOCKED", 2)
	behavior := held.Behaviors[0]
	if behavior.CurrentRunID != "NEWER-PASS" || behavior.Status != "FAIL" || !slices.ContainsFunc(behavior.Reasons, func(reason board.Reason) bool {
		return reason.Code == "MUST_PASS_FAILED" && reason.RunID == "EARLIER-FAIL" && reason.TestID == "T1" && reason.Attempt == 1
	}) {
		t.Fatal("a newer passing current run erased the earlier definitive failure")
	}
	frozen, err := app.Freeze(supervisor, project, first.ID)
	if err != nil || frozen.Status != "BLOCKED" {
		t.Fatalf("could not freeze the blocked original decision: %v", err)
	}
	metadata, html, err := app.ReportHTML(supervisor, project, frozen.ID)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := app.CommitImport(supervisor, project, staged.Token, failCSV)
	if err != nil || !reflect.DeepEqual(retry, original) {
		t.Fatalf("exact committed retry changed the original receipt: %v", err)
	}
	second := newCustodySuccessor(t, app, first, "a")
	if second.PredecessorID != first.ID {
		t.Fatal("new execution inputs did not retain the original lineage")
	}
	before := statusAssessment(t, app, second.ID, "BLOCKED", 2)
	if before.Behaviors[0].Status != "UNKNOWN" || len(before.Behaviors[0].Runs) != 0 {
		t.Fatal("successor inherited the predecessor's retained execution evidence")
	}
	importBytes(t, app, second.ID, csvBytes(t, resultRow(second, "T1", "1", "SUCCESSOR-PASS", "SUCCESSOR-RESULT", intakeCurrent, intakeCurrent, "PASS")))
	statusAssessment(t, app, second.ID, "SUFFICIENT", 3)
	after := statusAssessment(t, app, first.ID, "BLOCKED", 3)
	if after.Behaviors[0].Status != "FAIL" || len(after.Behaviors[0].Runs) != 2 {
		t.Error("successor's evidence changed the predecessor's failure history")
	}
	retained, raw, err := app.ReportHTML(supervisor, project, frozen.ID)
	if err != nil || retained != metadata || !bytes.Equal(html, raw) {
		t.Errorf("replay and successor changed the original frozen report: %v", err)
	}
}
