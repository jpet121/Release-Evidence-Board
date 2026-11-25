package board

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"testing"
)

func TestFollowupDerivedDraftVersionsFollowExactEditedFields(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	_, _, base, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name                             string
		edit                             func(*Behavior)
		behaviorRevision, policyRevision int
	}{
		{"same tests in reverse order", func(b *Behavior) { slices.Reverse(b.Tests) }, 1, 1},
		{"changed requiredness", func(b *Behavior) { b.Required = false }, 2, 1},
		{"changed behavior text", func(b *Behavior) { b.Text += " with the approved producer" }, 2, 1},
		{"changed expected result", func(b *Behavior) { b.Tests[1].ExpectedOutcome += " verified" }, 2, 1},
		{"changed age limit", func(b *Behavior) { b.Policy.MaxAgeHours++ }, 1, 2},
		{"changed lower mark", func(b *Behavior) { b.Policy.MinPassBasisPoints = 9000 }, 1, 2},
	}
	for _, entry := range cases {
		t.Run(entry.name, func(t *testing.T) {
			requested := base.Baseline
			requested.Behaviors = slices.Clone(requested.Behaviors)
			requested.Behaviors[0].Tests = slices.Clone(requested.Behaviors[0].Tests)
			entry.edit(&requested.Behaviors[0])
			draft, err := f.app.CreateDraft(secondActor, secondProject, requested, DraftOptions{FromBaseline: base.Descriptor.Digest})
			if err != nil {
				t.Fatal(err)
			}
			behavior := draft.Baseline.Behaviors[0]
			if draft.Baseline.Revision != base.Baseline.Revision+1 || behavior.Revision != entry.behaviorRevision || behavior.Policy.Revision != entry.policyRevision {
				t.Errorf("derived draft changed the wrong baseline, behavior or policy version: %d/%d/%d", draft.Baseline.Revision, behavior.Revision, behavior.Policy.Revision)
			}
			cloned, err := f.app.CreateDraft(secondActor, secondProject, draft.Baseline, DraftOptions{FromDraft: draft.ID})
			if err != nil || cloned.ParentDraft != draft.ID || cloned.Baseline.Revision != draft.Baseline.Revision+1 {
				t.Errorf("copy of exact retained draft did not advance its own baseline revision: %v", err)
			}
			if saved, err := f.app.Draft(secondActor, secondProject, draft.ID); err != nil || !reflect.DeepEqual(saved, draft) {
				t.Errorf("copy changed its independent retained source draft: %v", err)
			}
		})
	}
	if approved, err := f.app.Baseline(secondActor, secondProject, base.Descriptor.Digest); err != nil || !reflect.DeepEqual(approved, base) {
		t.Errorf("derived draft edits changed the approved baseline: %v", err)
	}
	if report, raw, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(raw, f.html) {
		t.Errorf("derived draft edits changed the prior frozen artifact: %v", err)
	}

	invalid := base.Baseline
	invalid.Behaviors = slices.Clone(invalid.Behaviors)
	invalid.Behaviors[0].Tests = slices.Clone(invalid.Behaviors[0].Tests)
	invalid.Behaviors[0].Tests[1].ID = invalid.Behaviors[0].Tests[0].ID
	draft, err := f.app.CreateDraft(secondActor, secondProject, invalid, DraftOptions{FromBaseline: base.Descriptor.Digest})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Approve(secondActor, secondProject, draft.ID, draft.Revision); !errors.Is(err, ErrInvalid) {
		t.Errorf("duplicated actionable test identity entered an approved roster: %v", err)
	}
	if read, err := f.app.Draft(secondActor, secondProject, draft.ID); err != nil || read.ApprovedDigest != "" || !reflect.DeepEqual(read.Baseline, draft.Baseline) {
		t.Errorf("approval refusal destroyed the editable retained draft: %v", err)
	}
}
