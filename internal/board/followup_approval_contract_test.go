package board_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

// A row is emitted only after its observable assertions complete.
func recordApprovalInstance(t *testing.T, instance string, parameters map[string]any, outcome string) {
	t.Helper()
	caseID, _, _ := strings.Cut(instance, ".")
	raw, err := json.Marshal(map[string]any{"id": instance, "case_id": caseID, "parameters": parameters, "actual": outcome})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("REB_ACCEPTANCE_INSTANCE " + string(raw))
}

func TestFollowupOrdinarySelfApprovalAudit(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	const actor = "contributor-reviewer"
	if err := app.SetAccount(supervisor, actor, testPassword, []board.Role{board.Contributor, board.Reviewer}); err != nil {
		t.Fatal(err)
	}
	draft, err := app.CreateDraft(actor, project, vectorBaseline(), board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	approval, err := app.Approve(actor, project, draft.ID, draft.Revision)
	if err != nil || !approval.SelfApproval || approval.Author != actor || approval.Approver != actor || len(approval.Thresholds) != 0 {
		t.Fatalf("ordinary self-approval did not preserve its author and reviewer: %#v %v", approval, err)
	}
	events, err := app.Audit(supervisor, project)
	if err != nil || !slices.ContainsFunc(events, func(event board.AuditEvent) bool {
		return event.Action == "baseline.approve" && event.Outcome == "success" && event.Actor == actor &&
			event.Time.Equal(fixtureTime) && slices.Equal(event.References, []string{draft.ID, actor, approval.Descriptor.Digest})
	}) {
		t.Fatalf("audit did not attribute the exact self-approval: %v", err)
	}
	recordApprovalInstance(t, "AC-001-07.P001", map[string]any{}, "Permitted with self-approval identified in audit")
}

func TestFollowupApprovalChecksExactRosterLimits(t *testing.T) {
	type specimen struct {
		id    string
		param map[string]any
		valid bool
		edit  func(*board.Baseline)
	}
	tests := []specimen{
		{"AC-002-03.P001", map[string]any{}, false, func(b *board.Baseline) { b.Behaviors[0].Required = false }},
		{"AC-002-04.P001", map[string]any{}, false, func(b *board.Baseline) { b.Behaviors[0].Tests = nil }},
		{"AC-002-05.P001", map[string]any{}, false, func(b *board.Baseline) { b.Behaviors[0].Tests[0].MustPass = false }},
	}
	for _, n := range []struct {
		id, field string
		value     int
		valid     bool
		set       func(*board.Behavior, int)
	}{
		{"AC-002-06.P001", "thresholdBasisPoints", 0, true, setApprovalThreshold},
		{"AC-002-06.P002", "thresholdBasisPoints", 10000, true, setApprovalThreshold},
		{"AC-002-07.P001", "thresholdBasisPoints", -1, false, setApprovalThreshold},
		{"AC-002-07.P002", "thresholdBasisPoints", 10001, false, setApprovalThreshold},
		{"AC-002-08.P001", "plannedAttempts", 1, true, setApprovalAttempts},
		{"AC-002-08.P002", "plannedAttempts", 100, true, setApprovalAttempts},
		{"AC-002-09.P001", "plannedAttempts", 0, false, setApprovalAttempts},
		{"AC-002-09.P002", "plannedAttempts", 101, false, setApprovalAttempts},
		{"AC-002-10.P001", "maxAgeHours", 1, true, setApprovalAge},
		{"AC-002-10.P002", "maxAgeHours", 720, true, setApprovalAge},
		{"AC-002-11.P001", "maxAgeHours", 0, false, setApprovalAge},
		{"AC-002-11.P002", "maxAgeHours", 721, false, setApprovalAge},
	} {
		tests = append(tests, specimen{n.id, map[string]any{n.field: n.value}, n.valid, func(b *board.Baseline) {
			n.set(&b.Behaviors[0], n.value)
		}})
	}
	tests = append(tests,
		specimen{"AC-002-14.P001", map[string]any{"behaviors": 501, "plannedAttempts": 10000}, false,
			func(b *board.Baseline) { b.Behaviors = approvalCapacityRoster(501, 10000, false) }},
		specimen{"AC-002-14.P002", map[string]any{"behaviors": 500, "plannedAttempts": 10001}, false,
			func(b *board.Baseline) { b.Behaviors = approvalCapacityRoster(500, 10001, false) }},
		specimen{"AC-002-15.P001", map[string]any{"duplicateId": "behavior_id"}, false, func(b *board.Baseline) {
			second := additionalApprovalBehavior(true)
			second.ID = b.Behaviors[0].ID
			b.Behaviors = append(b.Behaviors, second)
		}},
		specimen{"AC-002-15.P002", map[string]any{"duplicateId": "test_id within a behavior"}, false, func(b *board.Baseline) {
			b.Behaviors[0].Tests = append(b.Behaviors[0].Tests, b.Behaviors[0].Tests[0])
		}},
		specimen{"AC-002-15.P003", map[string]any{"duplicateId": "test_id across behaviors"}, false, func(b *board.Baseline) {
			second := additionalApprovalBehavior(true)
			second.Tests[0].ID = b.Behaviors[0].Tests[0].ID
			b.Behaviors = append(b.Behaviors, second)
		}},
	)
	for _, item := range []struct {
		id, field string
		scalars   int
		testText  bool
	}{
		{"AC-002-16.P001", "behavior text", 0, false},
		{"AC-002-16.P002", "behavior text", 4001, false},
		{"AC-002-16.P003", "expected outcome", 0, true},
		{"AC-002-16.P004", "expected outcome", 2001, true},
	} {
		tests = append(tests, specimen{item.id, map[string]any{"field": item.field, "unicodeScalars": item.scalars}, false, func(b *board.Baseline) {
			text := strings.Repeat("é", item.scalars)
			if item.testText {
				b.Behaviors[0].Tests[0].ExpectedOutcome = text
			} else {
				b.Behaviors[0].Text = text
			}
		}})
	}
	for _, item := range []struct {
		id        string
		condition string
		valid     bool
		change    func(*board.Behavior)
	}{
		{"AC-002-17.P001", "zero planned tests", false, func(b *board.Behavior) { b.Tests = nil }},
		{"AC-002-17.P002", "invalid behavior revision", false, func(b *board.Behavior) { b.Revision = 0 }},
		{"AC-002-17.P003", "invalid policy revision", false, func(b *board.Behavior) { b.Policy.Revision = 0 }},
		{"AC-002-17.P004", "valid optional roster has tests and no must-pass test", true, func(b *board.Behavior) { b.Tests[0].MustPass = false }},
	} {
		parameters := map[string]any{"condition": item.condition}
		if item.id == "AC-002-17.P002" || item.id == "AC-002-17.P003" {
			parameters["value"] = 0
		}
		tests = append(tests, specimen{item.id, parameters, item.valid, func(b *board.Baseline) {
			optional := additionalApprovalBehavior(false)
			item.change(&optional)
			b.Behaviors = append(b.Behaviors, optional)
		}})
	}
	tests = append(tests,
		specimen{"AC-002-18.P001", map[string]any{"requiredPlusOptionalAttempts": 10000}, true,
			func(b *board.Baseline) { b.Behaviors = approvalCapacityRoster(500, 10000, true) }},
		specimen{"AC-002-18.P002", map[string]any{"requiredPlusOptionalAttempts": 10001}, false,
			func(b *board.Baseline) { b.Behaviors = approvalCapacityRoster(500, 10001, true) }},
	)
	for _, item := range tests {
		t.Run(item.id, func(t *testing.T) {
			app, _ := openBoard(t, func() time.Time { return fixtureTime })
			contents := vectorBaseline()
			item.edit(&contents)
			checkApprovalRoster(t, app, contents, item.valid)
			caseID, _, _ := strings.Cut(item.id, ".")
			contract := map[string]string{
				"AC-002-03": "Rejected",
				"AC-002-04": "Rejected",
				"AC-002-05": "Rejected",
				"AC-002-06": "Both bounds accepted",
				"AC-002-07": "Each rejected",
				"AC-002-08": "Both bounds accepted",
				"AC-002-09": "Each rejected",
				"AC-002-10": "Both accepted",
				"AC-002-11": "Each rejected",
				"AC-002-14": "Rejected",
				"AC-002-15": "Rejected",
				"AC-002-16": "Rejected",
				"AC-002-17": "Rejected; valid optional behavior needs tests but need not have a must-pass test",
				"AC-002-18": "10000 accepted; 10001 rejected",
			}
			recordApprovalInstance(t, item.id, item.param, contract[caseID])
		})
	}
}

func checkApprovalRoster(t *testing.T, app *board.Board, contents board.Baseline, accepted bool) {
	t.Helper()
	draft, err := app.CreateDraft(supervisor, project, contents, board.DraftOptions{})
	var approved board.ApprovedBaseline
	if err == nil {
		approved, err = app.Approve(supervisor, project, draft.ID, draft.Revision)
	}
	all, pending, readError := app.Baselines(supervisor, project)
	if readError != nil {
		t.Fatal(readError)
	}
	if accepted {
		if err != nil || len(all) != 1 || approved.Descriptor.Digest == "" || all[0].Descriptor != approved.Descriptor {
			t.Fatalf("valid roster was not approved as one retained descriptor: %v", err)
		}
		retained, readError := app.Baseline(supervisor, project, approved.Descriptor.Digest)
		if readError != nil || retained.Approver != supervisor || retained.DraftID != draft.ID ||
			!sameApprovalRoster(retained.Baseline, contents) {
			t.Fatalf("valid roster was altered or lost: %v", readError)
		}
		return
	}
	if !errors.Is(err, board.ErrInvalid) || len(all) != 0 {
		t.Fatalf("invalid roster was promoted or rejected for another reason: approvals=%d error=%v", len(all), err)
	}
	for _, saved := range pending {
		if saved.ApprovedDigest != "" {
			t.Fatal("invalid roster left an approval on its draft")
		}
	}
}

func sameApprovalRoster(retained, expected board.Baseline) bool {
	if retained.ProductID != expected.ProductID || retained.ID != expected.ID || retained.Revision != expected.Revision ||
		len(retained.Behaviors) != len(expected.Behaviors) {
		return false
	}
	for index, got := range retained.Behaviors {
		want := expected.Behaviors[index]
		if got.ID != want.ID || got.Revision != want.Revision || got.Required != want.Required || got.Text != want.Text ||
			got.Policy != want.Policy || !slices.Equal(got.Tests, want.Tests) {
			return false
		}
	}
	return true
}

func setApprovalThreshold(b *board.Behavior, n int) { b.Policy.MinPassBasisPoints = n }
func setApprovalAttempts(b *board.Behavior, n int)  { b.Tests[0].PlannedAttempts = n }
func setApprovalAge(b *board.Behavior, n int)       { b.Policy.MaxAgeHours = n }

func additionalApprovalBehavior(required bool) board.Behavior {
	return board.Behavior{ID: "BEH2", Revision: 1, Required: required, Text: "Separate retained behavior",
		Policy: board.Policy{ID: "POL2", Revision: 1, MinPassBasisPoints: 6000, MaxAgeHours: 24},
		Tests:  []board.TestPlan{{ID: "T2", ExpectedOutcome: "Independent expected outcome", MustPass: true, PlannedAttempts: 1}}}
}

func approvalCapacityRoster(count, attempts int, optional bool) []board.Behavior {
	roster := make([]board.Behavior, 0, count)
	for i := 0; i < count; i++ {
		n := attempts / (count - i)
		b := additionalApprovalBehavior(!optional || i%2 == 0)
		b.ID = fmt.Sprintf("B-%03d", i)
		b.Policy.ID = fmt.Sprintf("P-%03d", i)
		b.Tests[0].ID = fmt.Sprintf("T-%03d", i)
		b.Tests[0].PlannedAttempts = n
		b.Tests[0].MustPass = b.Required
		roster = append(roster, b)
		attempts -= n
	}
	return roster
}

func TestFollowupPolicyAndContractChangesRequireNewApproval(t *testing.T) {
	for _, item := range []struct {
		id, field string
		change    func(*board.Baseline)
	}{
		{"AC-002-12.P001", "required", func(b *board.Baseline) { b.Behaviors[0].Required = false }},
		{"AC-002-12.P002", "expected_outcome", func(b *board.Baseline) { b.Behaviors[0].Tests[0].ExpectedOutcome = "Second expected outcome" }},
		{"AC-002-12.P003", "policy", func(b *board.Baseline) { b.Behaviors[0].Policy.MaxAgeHours = 22 }},
	} {
		t.Run(item.id, func(t *testing.T) {
			app, _ := openBoard(t, func() time.Time { return fixtureTime })
			initial := vectorBaseline()
			initial.Behaviors = append(initial.Behaviors, additionalApprovalBehavior(true))
			original := approveFixture(t, app, initial)
			item.change(&initial)
			pending, err := app.CreateDraft(supervisor, project, initial, board.DraftOptions{FromBaseline: original.Descriptor.Digest})
			if err != nil || pending.ApprovedDigest != "" {
				t.Fatalf("changed roster was implicitly approved: %v", err)
			}
			replacement, err := pending.Baseline.Descriptor()
			if err != nil || replacement.Digest == original.Descriptor.Digest {
				t.Fatalf("changed roster did not produce a distinct digest: %v", err)
			}
			if _, _, err := app.CreateCandidate(supervisor, project, board.CandidateInput{
				Manifest: vectorManifest(), BaselineDigest: replacement.Digest,
			}); !errors.Is(err, board.ErrNotFound) {
				t.Fatalf("unapproved roster could be assessed: %v", err)
			}
			approved, err := app.Approve(supervisor, project, pending.ID, pending.Revision)
			if err != nil || approved.Descriptor != replacement || approved.DraftID != pending.ID {
				t.Fatalf("new exact roster was not independently retained after approval: %v", err)
			}
			retained, err := app.Baseline(supervisor, project, original.Descriptor.Digest)
			if err != nil || retained.Descriptor != original.Descriptor || retained.DraftID != original.DraftID {
				t.Fatalf("approving a changed roster modified the original: %v", err)
			}
			recordApprovalInstance(t, item.id, map[string]any{"changedField": item.field}, "Each creates new digest and requires new approval")
		})
	}
}

func TestFollowupEditingApprovedTextLeavesBytesAndCreatesDraft(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	approved := approveFixture(t, app, vectorBaseline())
	original, err := app.Draft(supervisor, project, approved.DraftID)
	if err != nil {
		t.Fatal(err)
	}
	edited := vectorBaseline()
	edited.Behaviors[0].Text = "The revised text must stay outside the approved descriptor"
	if _, err := app.SaveDraft(supervisor, project, original.ID, original.Revision, edited); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("direct text edit changed an approved revision: %v", err)
	}
	next, err := app.CreateDraft(supervisor, project, edited, board.DraftOptions{FromBaseline: approved.Descriptor.Digest})
	if err != nil || next.ID == original.ID || next.Baseline.Revision != 2 || next.ParentBaseline != approved.Descriptor.Digest ||
		next.ApprovedDigest != "" {
		t.Fatalf("editing the approved baseline did not create a distinct unapproved revision: %v", err)
	}
	persisted, err := app.Draft(supervisor, project, next.ID)
	if err != nil || len(persisted.Baseline.Behaviors) != 1 || persisted.Baseline.Behaviors[0].Text != edited.Behaviors[0].Text {
		t.Fatalf("new revision did not retain the edited text: %v", err)
	}
	stored, err := app.Baseline(supervisor, project, approved.Descriptor.Digest)
	if err != nil || stored.Descriptor != approved.Descriptor || stored.Baseline.Behaviors[0].Text != "Reject unauthorized reads" {
		t.Fatalf("text editing changed the earlier approved bytes: %v", err)
	}
	recordApprovalInstance(t, "AC-002-01.P001", map[string]any{}, "Approved bytes unchanged; new draft revision created")
}
