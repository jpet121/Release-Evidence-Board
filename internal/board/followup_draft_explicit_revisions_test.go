package board

import (
	"bytes"
	"errors"
	"slices"
	"testing"
)

func TestFollowupDerivedDraftPreservesExplicitNewVersionIdentities(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	_, _, prior, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	options := DraftOptions{FromBaseline: prior.Descriptor.Digest}
	contents := prior.Baseline
	contents.ID = "independent-local-baseline"
	other, err := f.app.CreateDraft(secondActor, secondProject, contents, options)
	if err != nil || other.Baseline.ID != contents.ID || other.Baseline.Revision != 1 || other.ParentBaseline != prior.Descriptor.Digest {
		t.Fatalf("copying to an independent baseline ID changed its explicit first revision: %+v %v", other.Baseline, err)
	}
	contents = prior.Baseline
	contents.Revision += 7
	advanced, err := f.app.CreateDraft(secondActor, secondProject, contents, options)
	if err != nil || advanced.Baseline.Revision != contents.Revision || advanced.ParentBaseline != prior.Descriptor.Digest {
		t.Fatalf("copying an approval rewound the explicitly newer revision: %+v %v", advanced.Baseline, err)
	}
	contents.Revision += 5
	copied, err := f.app.CreateDraft(secondActor, secondProject, contents, DraftOptions{FromDraft: advanced.ID})
	if err != nil || copied.Baseline.Revision != contents.Revision || copied.ParentDraft != advanced.ID ||
		copied.ParentBaseline != prior.Descriptor.Digest {
		t.Fatalf("copying an editable draft rewound explicit newer or parent identities: %+v %v", copied, err)
	}

	for _, mode := range []string{"unfinished_behavior", "unfinished_policy", "already_newer_fields"} {
		t.Run(mode, func(t *testing.T) {
			input := prior.Baseline
			input.Behaviors = slices.Clone(input.Behaviors)
			changed := &input.Behaviors[0]
			switch mode {
			case "unfinished_behavior":
				changed.Revision = 0
				changed.Text = "An incomplete behavior edit"
			case "unfinished_policy":
				changed.Policy.Revision = 0
				changed.Policy.MaxAgeHours += 1
			case "already_newer_fields":
				changed.Revision += 3
				changed.Text = "An explicitly newer behavior revision"
				changed.Policy.Revision += 2
				changed.Policy.MaxAgeHours += 1
			}
			draft, err := f.app.CreateDraft(secondActor, secondProject, input, options)
			if err != nil || len(draft.Baseline.Behaviors) != 1 {
				t.Fatalf("proposed draft did not keep its editable roster: %v", err)
			}
			actual := draft.Baseline.Behaviors[0]
			if actual.Revision != changed.Revision || actual.Policy.Revision != changed.Policy.Revision {
				t.Fatalf("copy changed the explicit behavior or policy revisions: %+v", actual)
			}
			if mode != "already_newer_fields" {
				if _, err := f.app.Approve(secondActor, secondProject, draft.ID, draft.Revision); !errors.Is(err, ErrInvalid) {
					t.Fatalf("unfinished explicit revision escaped approval validation: %v", err)
				}
			}
		})
	}
}

func TestFollowupDraftCanAddIndependentBehaviorAfterSeparateRemoval(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	_, _, prior, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := f.app.CreateDraft(secondActor, secondProject, prior.Baseline, DraftOptions{FromBaseline: prior.Descriptor.Digest})
	if err != nil {
		t.Fatal(err)
	}
	empty := draft.Baseline
	empty.Behaviors = nil
	draft, err = f.app.SaveDraft(secondActor, secondProject, draft.ID, draft.Revision, empty)
	if err != nil || len(draft.Baseline.Behaviors) != 0 || !draft.InheritedBehavior("BH") {
		t.Fatalf("separate removal did not preserve the draft's inherited identity history: %v", err)
	}
	later := draft.Baseline
	later.Behaviors = []Behavior{{
		ID: "independent-added-behavior", Revision: 1, Required: true, Text: "Separate evidence intent after explicit removal",
		Policy: Policy{ID: "independent-policy", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
		Tests:  []TestPlan{{ID: "independent-required", MustPass: true, ExpectedOutcome: "Independent result", PlannedAttempts: 1}},
	}}
	draft, err = f.app.SaveDraft(secondActor, secondProject, draft.ID, draft.Revision, later)
	if err != nil || len(draft.Baseline.Behaviors) != 1 || draft.Baseline.Behaviors[0].ID != "independent-added-behavior" {
		t.Fatalf("later valid addition was treated as a simultaneous inherited rekey: %v", err)
	}
	if _, err := f.app.Approve(secondActor, secondProject, draft.ID, draft.Revision); err != nil {
		t.Fatalf("separate legitimate addition did not produce a verifiable approval: %v", err)
	}
	if report, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(html, f.html) {
		t.Fatalf("editing a derived draft changed the prior frozen report: %v", err)
	}
}
