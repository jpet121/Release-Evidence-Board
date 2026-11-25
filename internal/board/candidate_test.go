package board_test

import (
	"strings"
	"testing"

	"release-evidence-board/internal/board"
)

func approveFixture(t *testing.T, app *board.Board, baseline board.Baseline) board.ApprovedBaseline {
	t.Helper()
	draft, err := app.CreateDraft(supervisor, project, baseline, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := app.Approve(supervisor, project, draft.ID, draft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	return approved
}

func TestAC00313EquivalentCandidateAndAC00309Correction(t *testing.T) {
	app, _ := openBoard(t, nil)
	approved := approveFixture(t, app, vectorBaseline())
	input := board.CandidateInput{Manifest: vectorManifest(), BaselineDigest: approved.Descriptor.Digest, DisplayVersion: "Pilot v1"}
	first, reused, err := app.CreateCandidate(supervisor, project, input)
	if err != nil || reused {
		t.Fatalf("new candidate = %#v, reused %v, error %v", first, reused, err)
	}

	relabel := vectorBaseline()
	relabel.ID = "Relabeled"
	relabel.Behaviors[0].ID = "OtherBehaviorName"
	relabel.Behaviors[0].Policy.ID = "OtherPolicyName"
	relabel.Behaviors[0].Tests[0].ID = "OtherTestName"
	again := approveFixture(t, app, relabel)
	input.DisplayVersion = "Pilot v2"
	input.Manifest.ReleaseID = "R2"
	input.Manifest.ApplicationVersion = "2.0.0"
	input.BaselineDigest = again.Descriptor.Digest
	same, reused, err := app.CreateCandidate(supervisor, project, input)
	if err != nil || !reused || same.ID != first.ID {
		t.Fatalf("relabeled execution and roster created another candidate: %#v, reused %v, error %v", same, reused, err)
	}

	input.Manifest.BuildDigest = strings.Repeat("a", 64)
	input.BaselineDigest = first.BaselineDigest
	input.PredecessorID = first.ID
	input.Correction = "Corrected the test harness invocation in this build"
	next, reused, err := app.CreateCandidate(supervisor, project, input)
	if err != nil || reused || next.ID == first.ID || next.PredecessorID != first.ID {
		t.Fatalf("corrected candidate lost its predecessor or reused identity: %#v, reused %v, error %v", next, reused, err)
	}
}
