package board

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestFollowupDraftRejectsIdentityReplacementsBeforeWriting(t *testing.T) {
	app, original, _ := reviewBoard(t)
	draft, err := app.CreateDraft(secondActor, secondProject, original.Baseline, DraftOptions{FromBaseline: original.Descriptor.Digest})
	if err != nil {
		t.Fatal(err)
	}
	edits := []struct {
		name string
		edit func(*Baseline)
	}{
		{"product", func(b *Baseline) { b.ProductID = "DIFFERENT" }},
		{"identity", func(b *Baseline) { b.ID = "DIFFERENT" }},
		{"revision", func(b *Baseline) { b.Revision++ }},
		{"editable roster limit", func(b *Baseline) { b.Behaviors[0].Tests = make([]TestPlan, MaxAttempts+1) }},
	}
	for _, entry := range edits {
		t.Run(entry.name, func(t *testing.T) {
			proposed := draft.Baseline
			proposed.Behaviors = slices.Clone(proposed.Behaviors)
			entry.edit(&proposed)
			if _, err := app.SaveDraft(secondActor, secondProject, draft.ID, draft.Revision, proposed); !errors.Is(err, ErrInvalid) {
				t.Errorf("invalid draft edit was accepted: %v", err)
			}
			if after, err := app.Draft(secondActor, secondProject, draft.ID); err != nil || !reflect.DeepEqual(after, draft) {
				t.Errorf("refused edit changed the retained draft: %v", err)
			}
		})
	}
	rekeyed := original.Baseline
	rekeyed.Behaviors = slices.Clone(rekeyed.Behaviors)
	rekeyed.Behaviors[0].ID = "REPLACED"
	if replacement, err := app.CreateDraft(secondActor, secondProject, rekeyed, DraftOptions{
		FromBaseline: original.Descriptor.Digest,
	}); !errors.Is(err, ErrInvalid) || replacement.ID != "" {
		t.Errorf("a derived draft replaced an inherited behavior identity: %v", err)
	}
	if _, err := app.CreateDraft(secondActor, secondProject, original.Baseline, DraftOptions{FromDraft: "D-999"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("copy accepted an absent source draft: %v", err)
	}
	if _, err := app.SaveDraft(secondActor, secondProject, "D-999", 1, draft.Baseline); !errors.Is(err, ErrNotFound) {
		t.Errorf("save accepted an absent draft: %v", err)
	}
	approved, drafts, err := app.Baselines(secondActor, secondProject)
	if err != nil || !reflect.DeepEqual(approved, []ApprovedBaseline{original}) || len(drafts) != 2 {
		t.Errorf("draft refusals changed the approved or editable inventory: %v", err)
	}
}

func TestFollowupApprovalRejectsIndependentImmutableIdentityConflicts(t *testing.T) {
	app, original, _ := reviewBoard(t)
	cases := []struct {
		name string
		edit func(*Baseline)
	}{
		{"baseline revision", func(_ *Baseline) {}},
		{"behavior revision", func(b *Baseline) { b.ID = "B-BEHAVIOR"; b.Behaviors[0].Text = "Different behavior contract" }},
		{"policy revision", func(b *Baseline) { b.ID = "B-POLICY"; b.Behaviors[0].Policy.MaxAgeHours++ }},
		{"test ownership", func(b *Baseline) { b.ID = "B-OWNER"; b.Behaviors[0].ID = "BH-OTHER" }},
	}
	for _, entry := range cases {
		t.Run(entry.name, func(t *testing.T) {
			proposed := original.Baseline
			proposed.Behaviors = slices.Clone(proposed.Behaviors)
			entry.edit(&proposed)
			pending, err := app.CreateDraft(secondActor, secondProject, proposed, DraftOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if accepted, err := app.Approve(secondActor, secondProject, pending.ID, pending.Revision); !errors.Is(err, ErrConflict) || accepted.DraftID != "" {
				t.Errorf("approval reused an immutable identity: %v", err)
			}
			if after, err := app.Draft(secondActor, secondProject, pending.ID); err != nil || !reflect.DeepEqual(after, pending) {
				t.Errorf("conflicting approval changed the editable draft: %v", err)
			}
			if stored, err := app.Baseline(secondActor, secondProject, original.Descriptor.Digest); err != nil || !reflect.DeepEqual(stored, original) {
				t.Errorf("conflicting approval changed the original retained baseline: %v", err)
			}
		})
	}
	if err := app.Revalidate(secondActor, secondProject); err != nil {
		t.Errorf("a conflicting approval left an inconsistent index: %v", err)
	}
}

func TestFollowupCandidateAdmissionRequiresRetainedApprovedDigest(t *testing.T) {
	app, approved, _ := reviewBoard(t)
	prior := reviewCandidate(t, app, approved, "a")
	_, stored, _, err := app.Candidate(secondActor, secondProject, prior.ID)
	if err != nil {
		t.Fatal(err)
	}
	changed := stored.Manifest
	changed.BuildDigest = strings.Repeat("b", 64)
	if candidate, reused, err := app.CreateCandidate(secondActor, secondProject, CandidateInput{
		Manifest: changed, BaselineDigest: strings.Repeat("f", 64),
	}); !errors.Is(err, ErrNotFound) || reused || candidate.ID != "" {
		t.Errorf("candidate accepted an absent approved digest: %v", err)
	}
	if after, err := app.Candidates(secondActor, secondProject); err != nil || !reflect.DeepEqual(after, []Candidate{prior}) {
		t.Errorf("candidate refusal changed the retained catalog: %v", err)
	}
	if candidate, reused, err := app.CreateCandidate(secondActor, secondProject, CandidateInput{
		Manifest: changed, BaselineDigest: approved.Descriptor.Digest,
	}); err != nil || reused || candidate.ID == "" {
		t.Errorf("candidate admission did not recover with the exact approved reference: %v", err)
	}
}

func TestFollowupInvalidBootstrapLeavesTheSyntheticStoreAvailable(t *testing.T) {
	app, err := Open(filepath.Join(t.TempDir(), "synthetic.db"), Settings{ProjectID: secondProject, ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := app.Bootstrap("invalid local identifier", "synthetic-review-password"); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid account descriptor was accepted as the initial account: %v", err)
	}
	if err := app.Bootstrap(secondActor, "synthetic-review-password"); err != nil {
		t.Fatalf("invalid bootstrap changed the empty store: %v", err)
	}
	if err := app.RemoveAccount("unassigned", secondActor); !errors.Is(err, ErrForbidden) {
		t.Errorf("unassigned actor changed the account catalog: %v", err)
	}
	if account, err := app.Account(secondActor); err != nil || !account.CanOperate() || !account.CanReview() {
		t.Errorf("refusal changed the approved account: %v", err)
	}
}
