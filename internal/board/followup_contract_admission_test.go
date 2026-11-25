package board_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupContractAdmissionPreservesApprovedRoster(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	approved := approveFixture(t, app, vectorBaseline())
	before, beforeDrafts, err := app.Baselines(supervisor, project)
	if err != nil || !reflect.DeepEqual(before, []board.ApprovedBaseline{approved}) || len(beforeDrafts) != 1 {
		t.Fatalf("fixture lacks the approved roster and its retained draft: %v", err)
	}

	tooManyTests := vectorBaseline()
	tooManyTests.Behaviors[0].Tests = make([]board.TestPlan, 10001)
	foreignDraft := vectorBaseline()
	foreignDraft.ProductID = "OTHER"
	for _, entry := range []struct {
		name     string
		baseline board.Baseline
		options  board.DraftOptions
	}{
		{"foreign_product", foreignDraft, board.DraftOptions{}},
		{"over_10000_editable_tests", tooManyTests, board.DraftOptions{}},
		{"two_derivation_sources", vectorBaseline(), board.DraftOptions{FromBaseline: approved.Descriptor.Digest, FromDraft: approved.DraftID}},
	} {
		t.Run(entry.name, func(t *testing.T) {
			draft, err := app.CreateDraft(supervisor, project, entry.baseline, entry.options)
			if !errors.Is(err, board.ErrInvalid) || draft.ID != "" {
				t.Errorf("invalid request created a draft %q: %v", draft.ID, err)
			}
		})
	}

	input := board.CandidateInput{Manifest: vectorManifest(), BaselineDigest: approved.Descriptor.Digest}
	for _, entry := range []struct {
		name string
		edit func(*board.CandidateInput)
		want error
	}{
		{"foreign_product", func(v *board.CandidateInput) { v.Manifest.ProductID = "OTHER" }, board.ErrInvalid},
		{"invalid_display_text", func(v *board.CandidateInput) { v.DisplayVersion = string([]byte{0xff}) }, board.ErrInvalid},
		{"invalid_correction_text", func(v *board.CandidateInput) { v.Correction = string([]byte{0xff}) }, board.ErrInvalid},
		{"missing_explicit_predecessor", func(v *board.CandidateInput) { v.PredecessorID = "C-absent" }, board.ErrNotFound},
		{"correction_without_predecessor", func(v *board.CandidateInput) { v.Correction = "No retained predecessor" }, board.ErrInvalid},
	} {
		t.Run(entry.name, func(t *testing.T) {
			request := input
			entry.edit(&request)
			candidate, reused, err := app.CreateCandidate(supervisor, project, request)
			if !errors.Is(err, entry.want) || candidate.ID != "" || reused {
				t.Errorf("invalid request created or reused candidate %q: %v", candidate.ID, err)
			}
		})
	}
	found, err := app.Candidates(supervisor, project)
	if err != nil || len(found) != 0 {
		t.Fatalf("rejected admission changed the candidate catalog: %v", err)
	}
	baselines, drafts, err := app.Baselines(supervisor, project)
	if err != nil || !reflect.DeepEqual(baselines, before) || !reflect.DeepEqual(drafts, beforeDrafts) {
		t.Fatalf("rejected admission changed approved and editable inventories: %v", err)
	}
	if candidate, reused, err := app.CreateCandidate(supervisor, project, input); err != nil || reused || candidate.ID == "" {
		t.Errorf("untouched approved roster could not admit its eligible candidate: %v", err)
	}
}

func TestFollowupInferredPredecessorUsesExecutionIdentityAcrossReleaseLabels(t *testing.T) {
	for _, entry := range []struct {
		name string
		edit func(*board.Manifest)
	}{
		{"same_execution", func(*board.Manifest) {}},
		{"build", func(m *board.Manifest) { m.BuildDigest = strings.Repeat("b", 64) }},
		{"prompt", func(m *board.Manifest) { m.PromptDigest = strings.Repeat("c", 64) }},
		{"model", func(m *board.Manifest) { m.ModelConfigDigest = strings.Repeat("d", 64) }},
		{"knowledge", func(m *board.Manifest) { m.KnowledgeDigest = strings.Repeat("e", 64) }},
		{"environment", func(m *board.Manifest) { m.EnvironmentID = "OTHER-ENVIRONMENT" }},
	} {
		t.Run(entry.name, func(t *testing.T) {
			app, _ := openBoard(t, func() time.Time { return fixtureTime })
			first := fixtureCandidate(t, app, vectorBaseline())
			roster := vectorBaseline()
			roster.Behaviors[0].Text = "An independently updated approved behavior"
			draft, err := app.CreateDraft(supervisor, project, roster, board.DraftOptions{FromBaseline: first.BaselineDigest})
			if err != nil {
				t.Fatal(err)
			}
			updated, err := app.Approve(supervisor, project, draft.ID, draft.Revision)
			if err != nil || updated.Descriptor.Digest == first.BaselineDigest {
				t.Fatalf("new roster fixture did not change the approved material: %v", err)
			}
			manifest := vectorManifest()
			manifest.ReleaseID = "OTHER-RELEASE-LABEL"
			entry.edit(&manifest)
			next, reused, err := app.CreateCandidate(supervisor, project, board.CandidateInput{Manifest: manifest, BaselineDigest: updated.Descriptor.Digest})
			if err != nil || reused || next.ID == first.ID || next.ID == "" {
				t.Fatalf("changed material did not create an independent candidate: %v", err)
			}
			var want string
			if entry.name == "same_execution" {
				want = first.ID
			}
			if next.PredecessorID != want {
				t.Errorf("cross-release execution relationship differs from its changed input: got %q, want %q", next.PredecessorID, want)
			}
			if want != "" && !slices.Equal(next.Changes, []string{"Approved behavior, roster or policy"}) {
				t.Errorf("matching execution disclosed the wrong material changes: %v", next.Changes)
			}
			original, _, _, err := app.Candidate(supervisor, project, first.ID)
			if err != nil || !reflect.DeepEqual(original, first) {
				t.Errorf("inferred relationship changed the original candidate: %v", err)
			}
		})
	}
}

func TestFollowupAccountAdmissionPreservesAuthorizedAssignments(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	viewer := []board.Role{board.Viewer}
	before, err := app.Account(supervisor)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		name, id, password string
		roles              []board.Role
	}{
		{"invalid_identity", "bad id", testPassword, viewer},
		{"no_roles", "target", testPassword, nil},
		{"unknown_role", "target", testPassword, []board.Role{"unknown"}},
		{"duplicated_role", "target", testPassword, []board.Role{board.Viewer, board.Viewer}},
		{"eleven_bytes", "target", strings.Repeat("p", 11), viewer},
		{"seventy_three_bytes", "target", strings.Repeat("p", 73), viewer},
	} {
		t.Run(entry.name, func(t *testing.T) {
			if err := app.SetAccount(supervisor, entry.id, entry.password, entry.roles); !errors.Is(err, board.ErrInvalid) {
				t.Errorf("invalid role assignment did not refuse: %v", err)
			}
			if _, err := app.Account(entry.id); !errors.Is(err, board.ErrNotFound) {
				t.Errorf("invalid assignment was retained: %v", err)
			}
		})
	}
	if err := app.Bootstrap("different-actor", testPassword); !errors.Is(err, board.ErrConflict) {
		t.Errorf("second bootstrap took over the initialized board: %v", err)
	}
	if err := app.SetAccount(supervisor, supervisor, testPassword, viewer); !errors.Is(err, board.ErrInvalid) {
		t.Errorf("operator could demote their own role: %v", err)
	}
	if err := app.RemoveAccount(supervisor, supervisor); !errors.Is(err, board.ErrInvalid) {
		t.Errorf("operator could remove their own account: %v", err)
	}
	if err := app.RemoveAccount(supervisor, "missing-account"); !errors.Is(err, board.ErrNotFound) {
		t.Errorf("removal of an absent assignment did not refuse: %v", err)
	}
	if current, err := app.Authenticate(supervisor, testPassword); err != nil || !reflect.DeepEqual(current, before) {
		t.Fatalf("rejected assignments changed the valid operator: %v", err)
	}
	const id = "bounded-viewer"
	for _, count := range []int{12, 72} {
		password := strings.Repeat("z", count)
		if err := app.SetAccount(supervisor, id, password, viewer); err != nil {
			t.Fatalf("valid %d-byte assignment failed: %v", count, err)
		}
		if saved, err := app.Authenticate(id, password); err != nil || saved.ID != id || !slices.Equal(saved.Roles, viewer) {
			t.Errorf("valid %d-byte assignment did not preserve the requested role: %v", count, err)
		}
	}
}
