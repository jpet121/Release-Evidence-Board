package board

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"testing"
)

func TestFollowupDerivedLowerThresholdWithoutLocalEditRequiresAuthorIndependence(t *testing.T) {
	app, high, at := reviewBoard(t)
	if err := app.SetAccount(secondActor, draftAuthor, "synthetic-independent-fixture", []Role{Reviewer}); err != nil {
		t.Fatal(err)
	}
	lower := high.Baseline
	lower.Behaviors = slices.Clone(lower.Behaviors)
	lower.Behaviors[0].Policy.MinPassBasisPoints = 5000
	draft, err := app.CreateDraft(secondActor, secondProject, lower, DraftOptions{FromBaseline: high.Descriptor.Digest})
	if err != nil {
		t.Fatal(err)
	}
	approvedBeforeFailure, err := app.Approve(secondActor, secondProject, draft.ID, draft.Revision)
	if err != nil || len(approvedBeforeFailure.Thresholds) != 0 {
		t.Fatalf("lower policy was not independently established before any current historical failure: %v", err)
	}
	candidate := reviewCandidate(t, app, high, "a")
	receipt, _, source := reviewCommit(t, app, candidate, reviewRows(candidate, "a", "HIGH", Fail, at))
	old, err := app.Freeze(secondActor, secondProject, candidate.ID)
	if err != nil || old.Status != Blocked {
		t.Fatalf("stronger contract did not retain a definitive failure: %q, %v", old.Status, err)
	}
	_, oldHTML, err := app.ReportHTML(secondActor, secondProject, old.ID)
	if err != nil || len(oldHTML) == 0 {
		t.Fatal(err)
	}
	draft, err = app.CreateDraft(secondActor, secondProject, approvedBeforeFailure.Baseline, DraftOptions{FromBaseline: approvedBeforeFailure.Descriptor.Digest})
	if err != nil || len(draft.ThresholdEdits) != 0 || draft.Author != secondActor {
		t.Fatalf("derived draft did not preserve a known author with no local reduction edit: %v", err)
	}
	want := []ThresholdChange{{CandidateID: candidate.ID, BehaviorID: "BH", Before: 10000, After: 5000, ReducingActors: []string{secondActor}}}
	if changes, err := app.ReviewThresholds(secondActor, secondProject, draft.ID); err != nil || !reflect.DeepEqual(changes, want) {
		t.Fatalf("known draft author was not identified for the newly relevant reduction: %v, %v", changes, err)
	}
	if _, err := app.Approve(secondActor, secondProject, draft.ID, draft.Revision); !errors.Is(err, ErrForbidden) {
		t.Errorf("derived draft author approved their own newly relevant reduction: %v", err)
	}
	approved, err := app.Approve(draftAuthor, secondProject, draft.ID, draft.Revision)
	if err != nil || !reflect.DeepEqual(approved.Thresholds, want) || approved.Approver != draftAuthor {
		t.Fatalf("independent reviewer could not approve this exact narrowing: %v", err)
	}
	if report := freezeHalfPassingSuccessor(t, app, candidate, approved, at); report.Status != Sufficient {
		t.Errorf("independent narrowing did not freeze the accepted successor: %q", report.Status)
	}
	if report, html, err := app.ReportHTML(secondActor, secondProject, old.ID); err != nil || report != old || !bytes.Equal(html, oldHTML) {
		t.Errorf("new approval changed the independently frozen stronger result: %v", err)
	}
	if raw, err := app.SourceCSV(secondActor, secondProject, receipt.ID); err != nil || !bytes.Equal(raw, source) {
		t.Errorf("new approval changed the original stronger evidence: %v", err)
	}
}
