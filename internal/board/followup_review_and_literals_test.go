package board_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

// AC-001-08, AC-005-04 and AC-005-13. Separate the reviewer who can approve
// a lower threshold from both reducing actors, then verify the receipt and
// assessment against each exact contract.
func TestFollowupIndependentApprovalCannotReassignHistoricalNoOpEvidence(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	const peer, independent = "peer-reviewer", "third-reviewer"
	for _, actor := range []string{peer, independent} {
		if err := app.SetAccount(supervisor, actor, testPassword, []board.Role{board.Reviewer}); err != nil {
			t.Fatal(err)
		}
	}
	high := vectorBaseline()
	high.Behaviors[0].Tests = append(high.Behaviors[0].Tests,
		board.TestPlan{ID: "T2", ExpectedOutcome: "Additional check completed", PlannedAttempts: 1})
	old := fixtureCandidate(t, app, high)
	const at = "2026-11-17T12:00:00Z"
	oldRaw := csvBytes(t,
		resultRow(old, "T1", "1", "HIGHER", "HIGH-PASS", at, at, "PASS"),
		resultRow(old, "T2", "1", "HIGHER", "HIGH-FAIL", at, at, "FAIL"))
	oldReceipt := importBytes(t, app, old.ID, oldRaw)
	oldReport, err := app.Freeze(supervisor, project, old.ID)
	if err != nil || oldReport.Status != "BLOCKED" {
		t.Fatalf("higher threshold did not retain its exact failure: %v", err)
	}
	_, oldHTML, err := app.ReportHTML(supervisor, project, oldReport.ID)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := app.CreateDraft(supervisor, project, high, board.DraftOptions{FromBaseline: old.BaselineDigest})
	if err != nil {
		t.Fatal(err)
	}
	for _, reduction := range []struct {
		actor string
		mark  int
	}{{supervisor, 7500}, {peer, 5000}} {
		contents := draft.Baseline
		contents.Behaviors = slices.Clone(contents.Behaviors)
		contents.Behaviors[0].Policy.MinPassBasisPoints = reduction.mark
		draft, err = app.SaveDraft(reduction.actor, project, draft.ID, draft.Revision, contents)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, reducer := range []string{supervisor, peer} {
		if _, err := app.Approve(reducer, project, draft.ID, draft.Revision); !errors.Is(err, board.ErrForbidden) {
			t.Fatalf("reducing actor %q approved a threshold after the historical failure: %v", reducer, err)
		}
	}
	approved, err := app.Approve(independent, project, draft.ID, draft.Revision)
	if err != nil || approved.Approver != independent || len(approved.Thresholds) != 1 ||
		approved.Thresholds[0].CandidateID != old.ID || approved.Thresholds[0].Before != 10000 || approved.Thresholds[0].After != 5000 ||
		len(approved.Thresholds[0].ReducingActors) != 2 || !slices.Contains(approved.Thresholds[0].ReducingActors, supervisor) ||
		!slices.Contains(approved.Thresholds[0].ReducingActors, peer) {
		t.Fatalf("independent approval lost its exact threshold provenance: %v", err)
	}
	current, reused, err := app.CreateCandidate(supervisor, project, board.CandidateInput{
		Manifest: vectorManifest(), BaselineDigest: approved.Descriptor.Digest, PredecessorID: old.ID,
	})
	if err != nil || reused || current.ID == old.ID || current.PredecessorID != old.ID {
		t.Fatalf("lowered policy did not create a distinct candidate linked to the older failure: %v", err)
	}
	quoted := bytes.Replace(oldRaw, []byte(",FAIL,"), []byte(`,"FAIL",`), 1)
	if bytes.Equal(quoted, oldRaw) {
		t.Fatal("fixture CSV was not re-quoted")
	}
	pending := previewFollowupFile(t, app, current.ID, quoted)
	if !slices.Equal(pending.HistoricalCandidates, []string{old.ID}) {
		t.Fatal("historical upload lost its producer-bound older identity")
	}
	noop, err := app.CommitImport(supervisor, project, pending.Token, quoted)
	if err != nil || noop.ID == oldReceipt.ID || noop.LedgerHighWater != 1 || len(noop.AddedRuns) != 0 ||
		!slices.Equal(noop.ExistingRuns, []string{"HIGHER"}) || !slices.Equal(noop.PreviousReceipts, []string{oldReceipt.ID}) {
		t.Fatalf("quoted older bytes did not produce the expected no-op receipt: %v", err)
	}
	before, err := app.Assess(supervisor, project, current.ID)
	if err != nil || before.Status != "BLOCKED" || before.Behaviors[0].Status != "UNKNOWN" {
		t.Fatalf("historical receipt satisfied a replacement pair: %s, %v", before.Status, err)
	}
	freshPass := resultRow(current, "T1", "1", "LOWER", "LOW-PASS", at, at, "PASS")
	freshFail := resultRow(current, "T2", "1", "LOWER", "LOW-FAIL", at, at, "FAIL")
	freshPass[10], freshFail[10] = "3", "3"
	newReceipt := importBytes(t, app, current.ID, csvBytes(t, freshPass, freshFail))
	if newReceipt.LedgerHighWater != 2 {
		t.Fatal("new candidate evidence did not advance the ledger exactly once")
	}
	replayed, err := app.CommitImport(supervisor, project, pending.Token, quoted)
	if err != nil || replayed.ID != noop.ID || replayed.LedgerHighWater != 1 {
		t.Fatalf("exact stale no-op retry did not recover its original receipt: %v", err)
	}
	newReport, err := app.Freeze(independent, project, current.ID)
	if err != nil || newReport.Status != "SUFFICIENT" || newReport.LedgerHighWater != 2 {
		t.Fatalf("fresh half-passing evidence did not satisfy its independently approved pair: %s, %v", newReport.Status, err)
	}
	_, html, err := app.ReportHTML(supervisor, project, newReport.ID)
	if err != nil || !bytes.Contains(html, []byte("10000 to 5000 basis points")) ||
		!bytes.Contains(html, []byte("approved by third-reviewer at")) || bytes.Contains(html, []byte("HIGH-FAIL")) {
		t.Fatal("new frozen snapshot omitted approval facts or carried old evidence across identities")
	}
	_, preserved, err := app.ReportHTML(supervisor, project, oldReport.ID)
	if err != nil || !bytes.Equal(oldHTML, preserved) {
		t.Fatalf("approval and new evidence changed the earlier frozen report: %v", err)
	}
	t.Log("AC-001-08 reducers_denied=2 independent_approver=third-reviewer; AC-005-04 historical_quoting=noop ledger=1; AC-005-13 no_op_receipt_reused=true; old=BLOCKED new=SUFFICIENT")
}

// SPEC-2, AC-001-01/02/04/06. Exercise grouped role keywords individually
// through persisted assignments, authentication and actual operations.
func TestFollowupEachAssignedRoleHasItsOwnAuthority(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	candidate := fixtureCandidate(t, app, vectorBaseline())
	for _, assigned := range []struct {
		role                          board.Role
		read, draft, review, recovery bool
	}{
		{"viewer", true, false, false, false},
		{"contributor", true, true, false, false},
		{"reviewer", true, true, true, false},
		{"operator", false, false, false, true},
		{"release-owner", true, false, false, false},
	} {
		t.Run(string(assigned.role), func(t *testing.T) {
			actor := "assigned-" + string(assigned.role)
			if err := app.SetAccount(supervisor, actor, testPassword, []board.Role{assigned.role}); err != nil {
				t.Fatal(err)
			}
			account, err := app.Authenticate(actor, testPassword)
			if err != nil || account.ID != actor || !slices.Equal(account.Roles, []board.Role{assigned.role}) {
				t.Fatalf("individual role did not survive assignment and authentication: %v", err)
			}
			_, _, _, err = app.Candidate(actor, project, candidate.ID)
			assertFollowupRoleOperation(t, "read", assigned.read, err)
			contents := vectorBaseline()
			contents.ID = "draft-" + actor
			_, err = app.CreateDraft(actor, project, contents, board.DraftOptions{})
			assertFollowupRoleOperation(t, "draft", assigned.draft, err)
			contents.ID = "approve-" + actor
			pending, err := app.CreateDraft(supervisor, project, contents, board.DraftOptions{})
			if err != nil {
				t.Fatal(err)
			}
			approval, err := app.Approve(actor, project, pending.ID, pending.Revision)
			assertFollowupRoleOperation(t, "approve", assigned.review, err)
			if assigned.review && approval.Approver != actor {
				t.Fatal("authorized approval omitted assigned reviewer")
			}
			_, err = app.Health(actor, project)
			assertFollowupRoleOperation(t, "health", assigned.recovery, err)
			t.Logf("SPEC-2 role=%s read=%t draft=%t approve=%t health=%t", assigned.role, assigned.read, assigned.draft, assigned.review, assigned.recovery)
		})
	}
}

func assertFollowupRoleOperation(t *testing.T, operation string, allowed bool, err error) {
	t.Helper()
	if (allowed && err != nil) || (!allowed && !errors.Is(err, board.ErrForbidden)) {
		t.Errorf("operation=%s expected authorized=%t error=%v", operation, allowed, err)
	}
}

// C02 and AC-007-08. Verify each known CSV outcome from parser, through the
// committed run, to the assessment. Record reason values independently.
func TestFollowupEachAcceptedResultOutcomeHasItsSpecifiedMeaning(t *testing.T) {
	for _, outcome := range []struct{ input, candidate, behavior, reason, parameterSuffix string }{
		{"PASS", "SUFFICIENT", "PASS", "", ""},
		{"FAIL", "BLOCKED", "FAIL", "MUST_PASS_FAILED", ""},
		{"ERROR", "BLOCKED", "UNKNOWN", "ERROR_RESULT", ".P001"},
		{"SKIPPED", "BLOCKED", "UNKNOWN", "SKIPPED_RESULT", ".P002"},
		{"UNKNOWN", "BLOCKED", "UNKNOWN", "UNKNOWN_RESULT", ".P003"},
	} {
		t.Run(outcome.input, func(t *testing.T) {
			app, _ := openBoard(t, func() time.Time { return fixtureTime })
			candidate := fixtureCandidate(t, app, vectorBaseline())
			const at = "2026-11-17T12:00:00Z"
			raw := csvBytes(t, resultRow(candidate, "T1", "1", "OBSERVED", "RES1", at, at, outcome.input))
			receipt := importBytes(t, app, candidate.ID, raw)
			assessment, err := app.Assess(supervisor, project, candidate.ID)
			if err != nil || len(assessment.Behaviors) != 1 || assessment.Status != outcome.candidate || receipt.LedgerHighWater != 1 {
				t.Fatalf("literal %s yielded unexpected candidate=%s error=%v", outcome.input, assessment.Status, err)
			}
			behavior := assessment.Behaviors[0]
			if behavior.Status != outcome.behavior || behavior.CurrentRunID != "OBSERVED" {
				t.Fatal("admitted result was lost or received a wrong primary status")
			}
			if outcome.reason == "" {
				if len(behavior.Reasons) != 0 {
					t.Fatal("fresh passing evidence received an unexpected reason")
				}
			} else if !slices.ContainsFunc(behavior.Reasons, func(reason board.Reason) bool {
				return reason.Code == outcome.reason && reason.RunID == "OBSERVED" && reason.TestID == "T1" && reason.Attempt == 1
			}) {
				t.Fatalf("accepted %s did not disclose %s at the observed slot", outcome.input, outcome.reason)
			}
			if outcome.parameterSuffix != "" {
				stored, err := app.SourceCSV(supervisor, project, receipt.ID)
				if err != nil || !bytes.Equal(stored, raw) || len(behavior.Runs) != 1 || behavior.Runs[0].Definitive ||
					len(behavior.Runs[0].Rows) != 1 || behavior.Runs[0].Rows[0].Outcome() != outcome.input {
					t.Fatalf("submitted %s is not retained as the only current nondefinitive result: %v", outcome.input, err)
				}
				params := map[string]any{"resultOutcome": outcome.input}
				recordApprovalInstance(t, "AC-004-03"+outcome.parameterSuffix, params, "Stored; current state UNKNOWN absent definitive failure")
				recordApprovalInstance(t, "AC-007-08"+outcome.parameterSuffix, params, "UNKNOWN with corresponding reason")
			}
			t.Logf("C02 outcome=%s behavior=%s candidate=%s reason=%s", outcome.input, outcome.behavior, outcome.candidate, outcome.reason)
		})
	}
}
