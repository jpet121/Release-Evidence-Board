package board_test

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupAuthorizedMaterialAuditAttributesExactActorTimeAndRecord(t *testing.T) {
	now := fixtureTime
	app, _ := openBoard(t, func() time.Time { return now })
	const combined = "combined-contributor-reviewer"
	if err := app.SetAccount(supervisor, combined, testPassword, []board.Role{board.Contributor, board.Reviewer}); err != nil {
		t.Fatal(err)
	}
	draft, err := app.CreateDraft(combined, project, vectorBaseline(), board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := app.Approve(combined, project, draft.ID, draft.Revision)
	if err != nil || !approved.SelfApproval || approved.Author != combined || approved.Approver != combined ||
		!approved.ApprovedAt.Equal(now) || approved.DraftID != draft.ID {
		t.Fatalf("ordinary outside-D20 self-review authority or attribution absent: %v", err)
	}
	candidate, reused, err := app.CreateCandidate(combined, project, board.CandidateInput{
		Manifest: vectorManifest(), BaselineDigest: approved.Descriptor.Digest,
	})
	if err != nil || reused {
		t.Fatalf("fixture did not record creation of a new candidate: %v", err)
	}
	events := intakeAudit(t, app)
	assertMaterialEvent(t, events, "access.set", supervisor, "success", now, combined)
	assertMaterialEvent(t, events, "baseline.approve", combined, "success", now, draft.ID, combined, approved.Descriptor.Digest)
	assertMaterialEvent(t, events, "candidate.create", combined, "success", now, candidate.ID)
	now = now.Add(180 * 24 * time.Hour)
	scope := []string{candidate.ID}
	plan, err := app.PreviewPurge(supervisor, project, scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Purge(supervisor, project, scope, plan.Confirmation); err != nil {
		t.Fatalf("eligible candidate purge did not complete: %v", err)
	}
	assertMaterialEvent(t, intakeAudit(t, app), "retention.purge", supervisor, "success", now, candidate.ID)
	recordCorroboratedBoardObservation(t)
}

func assertMaterialEvent(t *testing.T, events []board.AuditEvent, action, actor, outcome string, at time.Time, references ...string) {
	t.Helper()
	matches := 0
	for _, e := range events {
		if e.Action != action || e.Actor != actor || e.Outcome != outcome || !e.Time.Equal(at) || e.Time.Location() != time.UTC {
			continue
		}
		missing := slices.ContainsFunc(references, func(ref string) bool { return !slices.Contains(e.References, ref) })
		if !missing {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("material event %s for %s matched %d attributed records, want 1", action, actor, matches)
	}
}

func TestFollowupFailedAuthorizedDraftMutationAppendsOnlyFailure(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	contents := vectorBaseline()
	draft, err := app.CreateDraft(supervisor, project, contents, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	prior := intakeAudit(t, app)
	contents.Behaviors[0].Text = "Synthetic text on rejected stale edit"
	if _, err := app.SaveDraft(supervisor, project, draft.ID, draft.Revision+1, contents); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("valid authorized actor with stale draft did not get a conflict: %v", err)
	}
	current, err := app.Draft(supervisor, project, draft.ID)
	if err != nil || !reflect.DeepEqual(current, draft) {
		t.Fatalf("failed authorized edit altered the saved draft: %v", err)
	}
	after := intakeAudit(t, app)
	if len(after) != len(prior)+1 || !reflect.DeepEqual(after[:len(prior)], prior) {
		t.Fatal("authorized failure modified history or appended another event")
	}
	assertMaterialEvent(t, after[len(prior):], "draft.edit", supervisor, "failure", fixtureTime)
	if len(after[len(prior)].References) != 0 {
		t.Fatal("failed draft audit contains references to rejected payload")
	}
	recordCorroboratedBoardObservation(t)
}
