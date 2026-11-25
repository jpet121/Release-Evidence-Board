package board

import (
	"errors"
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupDraftFinalRevisionStaysReadableAndApprovable(t *testing.T) {
	app, original, at := reviewBoard(t)
	pending, err := app.CreateDraft(secondActor, secondProject, original.Baseline, DraftOptions{
		FromBaseline: original.Descriptor.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Start just below the representable limit, with a consistent draft binding.
	// The public save, refusal, reopen and approval still use the real store.
	pending.Revision = math.MaxUint64 - 1
	if err := app.db.Update(func(tx *bolt.Tx) error {
		return putBoundJSON(tx, draftBucket, draftMetaBucket, pending.ID, pending)
	}); err != nil {
		t.Fatal(err)
	}
	contents := pending.Baseline
	contents.Behaviors = slices.Clone(contents.Behaviors)
	contents.Behaviors[0].Text = "Final reviewed wording"
	final, err := app.SaveDraft(secondActor, secondProject, pending.ID, pending.Revision, contents)
	contents.Behaviors[0].Revision++
	if err != nil || final.Revision != math.MaxUint64 || !reflect.DeepEqual(final.Baseline, contents) {
		t.Fatalf("could not save the final valid draft revision: revision=%d, %v", final.Revision, err)
	}
	tooLate := final.Baseline
	tooLate.Behaviors = slices.Clone(tooLate.Behaviors)
	tooLate.Behaviors[0].Text = "Must never replace the final revision"
	if attempted, err := app.SaveDraft(secondActor, secondProject, final.ID, final.Revision, tooLate); !errors.Is(err, ErrInvalid) {
		t.Errorf("another save did not refuse exhausted draft revision: revision=%d, %v", attempted.Revision, err)
	}
	file := app.db.Path()
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(file, Settings{
		ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	saved, err := reopened.Draft(secondActor, secondProject, final.ID)
	if err != nil || !reflect.DeepEqual(saved, final) {
		t.Errorf("capacity refusal changed the final draft across reopen: revision=%d, %v", saved.Revision, err)
	}
	if held, err := reopened.AssessmentSuspended(secondActor, secondProject); err != nil || held {
		t.Errorf("draft revision capacity raised an integrity hold: held=%t, %v", held, err)
	}
	approved, err := reopened.Approve(secondActor, secondProject, final.ID, final.Revision)
	if err != nil || approved.DraftID != final.ID || !reflect.DeepEqual(approved.Baseline, final.Baseline) {
		t.Errorf("the retained final draft could not be approved: %v", err)
	}
	prior, err := reopened.Baseline(secondActor, secondProject, original.Descriptor.Digest)
	if err != nil || !reflect.DeepEqual(prior, original) {
		t.Errorf("final draft revision changed the prior approved baseline: %v", err)
	}
}
