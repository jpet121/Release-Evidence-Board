package board_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

const (
	project      = "pilot"
	supervisor   = "reviewer"
	testPassword = "synthetic-test-password"
)

func openBoard(t *testing.T, clock func() time.Time) (*board.Board, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evidence.db")
	app, err := board.Open(path, board.Settings{ProjectID: project, ProductID: "DEMO", Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := app.Bootstrap(supervisor, testPassword); err != nil {
		t.Fatal(err)
	}
	return app, path
}

func TestAC001And002ApproveImmutableBaseline(t *testing.T) {
	timeOfApproval := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	app, _ := openBoard(t, func() time.Time { return timeOfApproval })
	if err := app.SetAccount(supervisor, "viewer", testPassword, []board.Role{board.Viewer}); err != nil {
		t.Fatal(err)
	}
	draft, err := app.CreateDraft(supervisor, project, vectorBaseline(), board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Approve("viewer", project, draft.ID, draft.Revision); !errors.Is(err, board.ErrForbidden) {
		t.Fatalf("viewer approval = %v, want forbidden", err)
	}
	if _, err := app.Approve(supervisor, "foreign", draft.ID, draft.Revision); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("foreign approval = %v, want not found", err)
	}
	approved, err := app.Approve(supervisor, project, draft.ID, draft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Descriptor.Digest != vectorBaselineDigest || approved.Approver != supervisor || !approved.ApprovedAt.Equal(timeOfApproval) || !approved.SelfApproval {
		t.Fatalf("wrong approved baseline and attribution: %#v", approved)
	}

	revision := vectorBaseline()
	revision.Behaviors[0].Text = "Changed behavior text"
	if _, err := app.SaveDraft(supervisor, project, draft.ID, draft.Revision, revision); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("immutable revision edit = %v, want conflict", err)
	}
	edit, err := app.CreateDraft(supervisor, project, revision, board.DraftOptions{FromBaseline: approved.Descriptor.Digest})
	if err != nil {
		t.Fatal(err)
	}
	if edit.Baseline.Revision != 2 {
		t.Fatalf("derived baseline revision = %d, want 2", edit.Baseline.Revision)
	}
	original, err := app.Baseline(supervisor, project, vectorBaselineDigest)
	if err != nil || original.Descriptor != approved.Descriptor {
		t.Fatalf("earlier approved descriptor changed: %#v, %v", original, err)
	}
}

func TestAC00220ThresholdGuardUsesApprovalTimeAndCopyHistory(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	old := fixtureCandidate(t, app, vectorBaseline())
	if err := app.SetAccount(supervisor, "independent", testPassword, []board.Role{board.Reviewer}); err != nil {
		t.Fatal(err)
	}
	lowered := vectorBaseline()
	lowered.ID = "standalone"
	lowered.Behaviors[0].Policy.ID = "separate-policy"
	lowered.Behaviors[0].Policy.MinPassBasisPoints = 8700
	draft, err := app.CreateDraft(supervisor, project, lowered, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lowered.ID = "relabeled"
	copyOfDraft, err := app.CreateDraft("independent", project, lowered, board.DraftOptions{FromDraft: draft.ID})
	if err != nil {
		t.Fatal(err)
	}
	at := "2026-11-17T12:00:00Z"
	importBytes(t, app, old.ID, csvBytes(t, resultRow(old, "T1", "1", "RUN1", "RES1", at, at, "FAIL")))
	if _, err := app.Approve(supervisor, project, copyOfDraft.ID, copyOfDraft.Revision); !errors.Is(err, board.ErrForbidden) {
		t.Fatalf("reducing actor approved copy after a retained failure: %v", err)
	}
	approved, err := app.Approve("independent", project, copyOfDraft.ID, copyOfDraft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Approver != "independent" || len(approved.Thresholds) != 1 {
		t.Fatalf("approval lacks independent reviewer and change details: %#v", approved)
	}
	change := approved.Thresholds[0]
	if change.CandidateID != old.ID || change.BehaviorID != "BEH1" || change.Before != 10000 || change.After != 8700 ||
		len(change.ReducingActors) != 1 || change.ReducingActors[0] != supervisor {
		t.Fatalf("threshold change = %#v", change)
	}
	stillFailed, err := app.Assess(supervisor, project, old.ID)
	if err != nil || stillFailed.Status != board.Blocked || stillFailed.Behaviors[0].Status != board.Fail {
		t.Fatalf("prior failure changed: %#v, %v", stillFailed, err)
	}
}
