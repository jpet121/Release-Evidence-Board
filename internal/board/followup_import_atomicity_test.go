package board_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

const (
	intakeEarlier = "2026-11-17T10:00:00Z"
	intakeMiddle  = "2026-11-17T10:30:00Z"
	intakeCurrent = "2026-11-17T11:00:00Z"
)

type atomicIntake struct {
	app        *board.Board
	candidate  board.Candidate
	original   board.Receipt
	initialCSV []byte
	older      [][]string
	current    [][]string
}

func newAtomicIntake(t *testing.T) atomicIntake {
	t.Helper()
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	roster := vectorBaseline()
	roster.Behaviors[0].Tests[0].PlannedAttempts = 3
	candidate := fixtureCandidate(t, app, roster)
	older := intakeThreeRows(candidate, "OLD-INCOMPLETE", intakeEarlier, "PASS")[:2:2]
	current := intakeThreeRows(candidate, "CURRENT-PASS", intakeCurrent, "PASS")
	raw := csvBytes(t, append(slices.Clone(older), current...)...)
	receipt := importBytes(t, app, candidate.ID, raw)
	if receipt.LedgerHighWater != 2 || len(receipt.AddedRuns) != 2 {
		t.Fatal("fixture did not commit exactly two independent initial runs")
	}
	return atomicIntake{app, candidate, receipt, raw, older, current}
}

func intakeThreeRows(candidate board.Candidate, run, finish, firstOutcome string) [][]string {
	return [][]string{
		resultRow(candidate, "T1", "1", run, run+"-1", finish, finish, firstOutcome),
		resultRow(candidate, "T1", "2", run, run+"-2", finish, finish, "PASS"),
		resultRow(candidate, "T1", "3", run, run+"-3", finish, finish, "PASS"),
	}
}

func TestFollowupRejectMixedExistingRunChangesWithoutPartialImport(t *testing.T) {
	fx := newAtomicIntake(t)
	changed := intakeThreeRows(fx.candidate, "CURRENT-PASS", intakeCurrent, "FAIL")
	extended := intakeThreeRows(fx.candidate, "OLD-INCOMPLETE", intakeEarlier, "PASS")
	for _, check := range []struct {
		name string
		rows [][]string
	}{
		{"changed-result", changed},
		{"retained-subset", fx.current[:2]},
		{"retained-extension", extended},
	} {
		t.Run(check.name, func(t *testing.T) {
			fx.rejectMixed(t, "IMMUTABLE_RUN", "run_id", 5, check.rows...)
			recordCorroboratedBoardObservation(t)
		})
	}

	// This pair is healthy: original exact retained rows plus one older new FAIL.
	// Only the latter receives a new sequence. The retained newer PASS still
	// supplies current evidence, but cannot erase the newly discovered failure.
	newRun := intakeThreeRows(fx.candidate, "HISTORICAL-FAIL", intakeMiddle, "FAIL")
	raw := csvBytes(t, append(slices.Clone(fx.current), newRun...)...)
	before := intakeAudit(t, fx.app)
	receipt := importBytes(t, fx.app, fx.candidate.ID, raw)
	if receipt.LedgerHighWater != 3 || receipt.Rows != 6 ||
		!slices.Equal(receipt.AddedRuns, []string{"HISTORICAL-FAIL"}) ||
		!slices.Equal(receipt.ExistingRuns, []string{"CURRENT-PASS"}) ||
		!slices.Equal(receipt.PreviousReceipts, []string{fx.original.ID}) ||
		!slices.Equal(receipt.CandidateIDs, []string{fx.candidate.ID}) {
		t.Fatalf("mixed valid file did not distinguish retained from new runs: %+v", receipt)
	}
	stored, err := fx.app.SourceCSV(supervisor, project, receipt.ID)
	if err != nil || !bytes.Equal(stored, raw) {
		t.Fatalf("new receipt lost exact submitted bytes: %v", err)
	}
	after := intakeAudit(t, fx.app)
	if len(after) != len(before)+1 || !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatal("valid mixed import did not append exactly one material audit event")
	}
	event := after[len(before)]
	if event.Action != "import.commit" || event.Outcome != "success" || event.Actor != supervisor ||
		!event.Time.Equal(fixtureTime) || !slices.Contains(event.References, receipt.ID) {
		t.Fatalf("valid mixed import audit missing its attribution or references: %+v", event)
	}
	assessment, err := fx.app.Assess(supervisor, project, fx.candidate.ID)
	if err != nil || assessment.Status != "BLOCKED" || assessment.LedgerHighWater != 3 ||
		len(assessment.Behaviors) != 1 || assessment.Behaviors[0].Status != "FAIL" ||
		assessment.Behaviors[0].CurrentRunID != "CURRENT-PASS" {
		t.Fatalf("valid new historical failure did not block a later passing run: %s, %v", assessment.Status, err)
	}
	t.Log("AC-005-10 existing=1 new=1 ledger=3 source=exact audit=attributed-success persistent_fail=BLOCKED")
	recordCorroboratedBoardObservation(t)
}

func TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically(t *testing.T) {
	fx := newAtomicIntake(t)
	otherRoster := vectorBaseline()
	otherRoster.ID = "SECOND-ROSTER"
	otherBehavior := &otherRoster.Behaviors[0]
	otherBehavior.ID, otherBehavior.Text = "SECOND-BEHAVIOR", "Genuinely distinct expected behavior"
	otherBehavior.Policy.ID, otherBehavior.Tests[0].ID = "SECOND-POLICY", "SECOND-TEST"
	other := approveFixture(t, fx.app, otherRoster)
	manifest := vectorManifest()
	manifest.BuildDigest = strings.Repeat("b", 64)
	otherCandidate, reused, err := fx.app.CreateCandidate(supervisor, project, board.CandidateInput{
		Manifest: manifest, BaselineDigest: other.Descriptor.Digest,
	})
	if err != nil || reused || otherCandidate.ID == fx.candidate.ID {
		t.Fatalf("second known pair fixture not distinct: %v, reused=%t", err, reused)
	}
	run := runOnly(fx.candidate, "INVALID-SUBMISSION", intakeMiddle)
	result := resultRow(fx.candidate, "T1", "1", "INVALID-SUBMISSION", "INVALID-ROW", intakeMiddle, intakeMiddle, "PASS")
	type fieldCheck struct {
		name   string
		source []string
		index  int
		value  string
		code   string
	}
	tests := []fieldCheck{
		{"unknown-manifest", result, 5, strings.Repeat("c", 64), "UNKNOWN_PAIR"},
		{"inconsistent-release", result, 4, "OTHER-RELEASE", "RELEASE_MISMATCH"},
		{"individually-known-unbound-pair", result, 6, other.Descriptor.Digest, "UNKNOWN_PAIR"},
		{"cross-product", result, 3, "OTHER-PRODUCT", "PRODUCT"},
		{"result-invalid-close", result, 15, "2026-02-30T10:30:00Z", "TIMESTAMP"},
		{"result-leap-second-close", result, 15, "2026-11-17T10:29:60Z", "TIMESTAMP"},
	}
	for _, column := range []int{0, 1, 3, 4, 5, 6, 7, 8, 9, 10, 12, 15} {
		tests = append(tests, fieldCheck{"empty-run-only-" + strconv.Itoa(column+1), run, column, "", "REQUIRED_FIELD"})
	}
	for _, nonempty := range []struct {
		index int
		value string
	}{
		{2, "PROPOSED-ID"}, {11, "T1"}, {13, "1"}, {14, intakeMiddle},
		{16, "PASS"}, {17, "https://example.invalid/fixture"}, {18, "literal result excerpt"},
	} {
		tests = append(tests, fieldCheck{"populated-run-only-" + strconv.Itoa(nonempty.index+1), run, nonempty.index, nonempty.value, "RUN_ONLY_FIELD"})
	}
	for _, lexeme := range []string{"+1", "-1", "0", "01", "1e0"} {
		tests = append(tests, fieldCheck{"invalid-policy-revision-" + lexeme, result, 10, lexeme, "INTEGER"})
	}
	for _, column := range []int{3, 4, 7, 9, 11, 12} {
		tests = append(tests, fieldCheck{"overlong-identifier-" + strconv.Itoa(column+1), result, column, strings.Repeat("v", 81), "ID"})
	}
	headers := strings.Split(strings.TrimSpace(resultsHeader), ",")
	for _, check := range tests {
		t.Run(check.name, func(t *testing.T) {
			invalid := slices.Clone(check.source)
			invalid[check.index] = check.value
			column := headers[check.index]
			if check.name == "individually-known-unbound-pair" {
				// The diagnostic identifies the composite manifest/baseline
				// lookup via its manifest column.
				column = "manifest_digest"
			}
			fx.rejectMixed(t, check.code, column, 5, invalid)
			recordCorroboratedBoardObservation(t)
		})
	}
	t.Run("duplicate-run-only", func(t *testing.T) {
		fx.rejectMixed(t, "RUN_ONLY_MIXED", "record_type", 6, run, slices.Clone(run))
		recordCorroboratedBoardObservation(t)
	})
}

func (fx atomicIntake) rejectMixed(t *testing.T, code, column string, line int, invalid ...[]string) {
	t.Helper()
	prior, err := fx.app.Candidates(supervisor, project)
	if err != nil {
		t.Fatal(err)
	}
	before := intakeAudit(t, fx.app)
	rows := intakeThreeRows(fx.candidate, "A-NEW-FAIL", intakeMiddle, "FAIL")
	rows = append(rows, invalid...)
	rows = append(rows, intakeThreeRows(fx.candidate, "Z-NEW-FAIL", intakeMiddle, "FAIL")...)
	raw := csvBytes(t, rows...)
	preview, err := fx.app.PreviewImport(supervisor, project, fx.candidate.ID, raw)
	if err != nil || preview.Token != "" || len(preview.Projected) != 0 {
		t.Fatalf("invalid mixed upload obtained a committable preview: token=%t projected=%d error=%v", preview.Token != "", len(preview.Projected), err)
	}
	found := slices.ContainsFunc(preview.Issues, func(issue board.Diagnostic) bool {
		return issue.Code == code && issue.Column == column && issue.LogicalRow == line && issue.PhysicalLine == line
	})
	if !found {
		t.Fatalf("expected %s at row %d, field %s, got %+v", code, line, column, preview.Issues)
	}
	rejectedID := fmt.Sprintf("I-%x", sha256.Sum256(raw))
	if _, err := fx.app.Receipt(supervisor, project, rejectedID); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("rejected batch acquired a receipt: %v", err)
	}
	if _, err := fx.app.SourceCSV(supervisor, project, rejectedID); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("rejected batch exposed source bytes: %v", err)
	}
	current, err := fx.app.Candidates(supervisor, project)
	if err != nil || !reflect.DeepEqual(current, prior) {
		t.Fatalf("invalid mixed upload changed candidate identities: %v", err)
	}
	assessment, err := fx.app.Assess(supervisor, project, fx.candidate.ID)
	if err != nil || assessment.Status != "SUFFICIENT" || assessment.LedgerHighWater != 2 ||
		len(assessment.Behaviors) != 1 || len(assessment.Behaviors[0].Runs) != 2 ||
		assessment.Behaviors[0].CurrentRunID != "CURRENT-PASS" {
		t.Fatalf("rejected mixed batch changed the prior assessment: %s ledger=%d error=%v", assessment.Status, assessment.LedgerHighWater, err)
	}
	original, err := fx.app.SourceCSV(supervisor, project, fx.original.ID)
	if err != nil || !bytes.Equal(original, fx.initialCSV) {
		t.Fatalf("rejected batch changed original evidence: %v", err)
	}
	after := intakeAudit(t, fx.app)
	if len(after) <= len(before) || !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatal("rejection did not preserve the existing audit prefix and append a failure")
	}
	for _, event := range after[len(before):] {
		if event.Outcome == "success" {
			t.Fatalf("invalid mixed batch recorded successful mutation: %s", event.Action)
		}
	}
	t.Logf("diagnostic=%s column=%s row=%d live=SUFFICIENT ledger=2 receipt=absent source=unchanged audit=no-success", code, column, line)
}

func intakeAudit(t *testing.T, app *board.Board) []board.AuditEvent {
	t.Helper()
	events, err := app.Audit(supervisor, project)
	if err != nil {
		t.Fatal(err)
	}
	return events
}
