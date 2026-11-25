package board

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupApprovalRefusesLoweringAgainstUnreadableRunAndCannotRepeat(t *testing.T) {
	app, high, at := reviewBoard(t)
	if err := app.SetAccount(secondActor, draftAuthor, "synthetic-threshold-reviewer", []Role{Reviewer}); err != nil {
		t.Fatal(err)
	}
	candidate := reviewCandidate(t, app, high, "a")
	receipt, _, source := reviewCommit(t, app, candidate, reviewRows(candidate, "a", "FAILED-ORIGIN", Fail, at))
	report, err := app.Freeze(secondActor, secondProject, candidate.ID)
	if err != nil || report.Status != Blocked {
		t.Fatalf("definitive prior failure was not frozen: %+v %v", report, err)
	}
	_, frozen, err := app.ReportHTML(secondActor, secondProject, report.ID)
	if err != nil {
		t.Fatal(err)
	}
	lower := high.Baseline
	lower.Behaviors = slices.Clone(lower.Behaviors)
	lower.Behaviors[0].Policy.MinPassBasisPoints = 5000
	draft, err := app.CreateDraft(secondActor, secondProject, lower, DraftOptions{FromBaseline: high.Descriptor.Digest})
	if err != nil {
		t.Fatal(err)
	}
	expected := []ThresholdChange{{CandidateID: candidate.ID, BehaviorID: "BH", Before: 10000, After: 5000, ReducingActors: []string{secondActor}}}
	if changes, err := app.ReviewThresholds(draftAuthor, secondProject, draft.ID); err != nil || !reflect.DeepEqual(changes, expected) {
		t.Fatalf("fixture did not establish the actual lower threshold and known failed origin: %+v %v", changes, err)
	}
	var saved []byte
	run := []byte(receipt.AddedRuns[0])
	if err := app.db.Update(func(tx *bolt.Tx) error {
		runs := tx.Bucket([]byte(runBucket))
		saved = bytes.Clone(runs.Get(run))
		return runs.Put(run, []byte("{"))
	}); err != nil {
		t.Fatal(err)
	}
	if approval, err := app.Approve(draftAuthor, secondProject, draft.ID, draft.Revision); !errors.Is(err, ErrIntegrity) || approval.Descriptor.Digest != "" {
		t.Fatalf("approval ignored an unreadable run supporting threshold review: %q %v", approval.Descriptor.Digest, err)
	}
	if pending, err := app.Draft(draftAuthor, secondProject, draft.ID); err != nil || !reflect.DeepEqual(pending, draft) {
		t.Fatalf("refused approval changed the exact pending draft: %v", err)
	}
	if err := app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(runBucket)).Put(run, saved)
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.Revalidate(draftAuthor, secondProject); err != nil {
		t.Fatalf("repaired failed-run record could not be revalidated: %v", err)
	}
	approved, err := app.Approve(draftAuthor, secondProject, draft.ID, draft.Revision)
	if err != nil || !reflect.DeepEqual(approved.Thresholds, expected) {
		t.Fatalf("valid independent reviewer did not retain the exact lower threshold history: %+v %v", approved.Thresholds, err)
	}
	if duplicate, err := app.Approve(draftAuthor, secondProject, draft.ID, draft.Revision); !errors.Is(err, ErrConflict) || duplicate.Descriptor.Digest != "" {
		t.Fatalf("same revision published a second approval for this already approved draft: %q %v", duplicate.Descriptor.Digest, err)
	}
	if current, err := app.Baseline(draftAuthor, secondProject, approved.Descriptor.Digest); err != nil || !reflect.DeepEqual(current, approved) {
		t.Fatalf("duplicate approval attempt changed the retained record: %v", err)
	}
	if csv, err := app.SourceCSV(draftAuthor, secondProject, receipt.ID); err != nil || !bytes.Equal(csv, source) {
		t.Fatalf("approval attempts changed the exact failed original: %v", err)
	}
	if saved, html, err := app.ReportHTML(draftAuthor, secondProject, report.ID); err != nil || saved != report || !bytes.Equal(html, frozen) {
		t.Fatalf("approval attempts changed the prior frozen blocked status or evidence: %v", err)
	}
}

func TestFollowupThresholdReviewDistinguishesEvidenceIntentAndReducingEdits(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	app := f.app
	_, _, high, err := app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	failed := reviewCandidate(t, app, high, "b")
	reviewCommit(t, app, failed, reviewRows(failed, "b", "FAILED", Fail, f.at))
	if err := app.SetAccount(secondActor, draftAuthor, "synthetic-lowering-editor", []Role{Reviewer}); err != nil {
		t.Fatal(err)
	}
	alias := high.Baseline.Behaviors[0]
	alias.ID, alias.Policy.ID = "named-replacement", "P-replacement"
	alias.Tests = slices.Clone(alias.Tests)
	alias.Tests[0].ID, alias.Tests[1].ID = "named-T1", "named-T2"
	distinct := alias
	distinct.ID, distinct.Text, distinct.Policy.ID = "independent-behavior", "Independent evidence intent", "P-independent"
	distinct.Tests = []TestPlan{{ID: "independent-T1", ExpectedOutcome: "Independent result", MustPass: true, PlannedAttempts: 1}}

	optionalInput := high.Baseline
	optionalInput.Behaviors = append(slices.Clone(high.Baseline.Behaviors), distinct)
	optionalInput.Behaviors[0].Required = false
	optionalDraft, err := app.CreateDraft(secondActor, secondProject, optionalInput, DraftOptions{FromBaseline: high.Descriptor.Digest})
	if err != nil {
		t.Fatal(err)
	}
	optionalApproval, err := app.Approve(secondActor, secondProject, optionalDraft.ID, optionalDraft.Revision)
	if err != nil {
		t.Fatalf("valid roster with a distinct required row could not retain an optional behavior: %v", err)
	}
	reviewCandidate(t, app, optionalApproval, "c")
	optionalAlias := alias
	optionalAlias.Required, optionalAlias.Policy.MinPassBasisPoints = false, 5000
	optionalInput = optionalApproval.Baseline
	optionalInput.Behaviors = append(slices.Clone(optionalInput.Behaviors), optionalAlias)
	optionalInput.Revision = optionalApproval.Baseline.Revision + 1
	optionalReplacement, err := app.CreateDraft(secondActor, secondProject, optionalInput, DraftOptions{FromBaseline: optionalApproval.Descriptor.Digest})
	if err != nil {
		t.Fatal(err)
	}
	if changes, err := app.ReviewThresholds(secondActor, secondProject, optionalReplacement.ID); err != nil || len(changes) != 0 {
		t.Fatalf("optional replacement was treated as the new required evidence gate: %+v %v", changes, err)
	}

	for _, change := range []string{"distinct-text", "shorter-roster"} {
		t.Run(change, func(t *testing.T) {
			revised := alias
			revised.Policy.MinPassBasisPoints = 5000
			if change == "distinct-text" {
				revised.Text = "A separately reviewed outcome"
			} else {
				revised.Tests = revised.Tests[:1]
			}
			contents := Baseline{ID: change, Revision: 1, ProductID: "DEMO", Behaviors: []Behavior{revised}}
			draft, err := app.CreateDraft(secondActor, secondProject, contents, DraftOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if changes, err := app.ReviewThresholds(secondActor, secondProject, draft.ID); err != nil || len(changes) != 0 {
				t.Fatalf("a different evidence intent inherited the failed threshold: %+v %v", changes, err)
			}
		})
	}
	contents := Baseline{ID: "exact-replacement", Revision: 1, ProductID: "DEMO", Behaviors: []Behavior{alias, distinct}}
	draft, err := app.CreateDraft(secondActor, secondProject, contents, DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	contents.Behaviors[0].Policy.MinPassBasisPoints = 5000
	draft, err = app.SaveDraft(draftAuthor, secondProject, draft.ID, draft.Revision, contents)
	if err != nil {
		t.Fatal(err)
	}
	expected := []ThresholdChange{{CandidateID: failed.ID, BehaviorID: "BH", Before: 10000, After: 5000, ReducingActors: []string{draftAuthor}}}
	if changes, err := app.ReviewThresholds(secondActor, secondProject, draft.ID); err != nil || !reflect.DeepEqual(changes, expected) {
		t.Fatalf("aliased threshold did not identify only the exact failed candidate and lowering editor: %+v %v", changes, err)
	}
	if _, err := app.Approve(draftAuthor, secondProject, draft.ID, draft.Revision); !errors.Is(err, ErrForbidden) {
		t.Fatalf("actual lowering editor could approve their own threshold reduction: %v", err)
	}
	if accepted, err := app.Approve(secondActor, secondProject, draft.ID, draft.Revision); err != nil || !reflect.DeepEqual(accepted.Thresholds, expected) {
		t.Fatalf("non-lowering initial author was unable to give genuine independent approval: %+v %v", accepted.Thresholds, err)
	}

	derived, err := app.CreateDraft(secondActor, secondProject, high.Baseline, DraftOptions{FromBaseline: high.Descriptor.Digest})
	if err != nil {
		t.Fatal(err)
	}
	addition := derived.Baseline
	addition.Behaviors = append(slices.Clone(addition.Behaviors), distinct)
	if saved, err := app.SaveDraft(secondActor, secondProject, derived.ID, derived.Revision, addition); err != nil ||
		len(saved.Baseline.Behaviors) != 2 || !saved.InheritedBehavior("BH") {
		t.Fatalf("adding a valid derived behavior changed inherited behavior custody: %v", err)
	}
}
