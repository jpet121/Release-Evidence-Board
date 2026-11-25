package board

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestTicket80ThresholdReviewPreservesCatalogPriorityAndFrozenFailure(t *testing.T) {
	app, prior, draft, at := retainedHighMarkAndReducedDraft(t)
	_, _, approved, err := app.Candidate(secondActor, secondProject, prior.ID)
	if err != nil {
		t.Fatal(err)
	}
	unrelated := reviewCandidate(t, app, approved, "c")
	if unrelated.ID <= prior.ID {
		t.Fatal("fixture requires the corrupt descriptor after the failed candidate in catalog order")
	}
	// This second failed run must not cause a duplicate reduction for BH.
	receipt, _, source := reviewCommit(t, app, prior, reviewRows(prior, "a", "SECOND-FAILED", Fail, at))
	frozen, err := app.Freeze(secondActor, secondProject, prior.ID)
	if err != nil || frozen.Status != Blocked {
		t.Fatalf("fixture lacks frozen definitive failure: %+v %v", frozen, err)
	}
	_, html, err := app.ReportHTML(secondActor, secondProject, frozen.ID)
	if err != nil {
		t.Fatal(err)
	}
	original := make(map[string][]byte)
	editClosedStore := func(restore bool) {
		t.Helper()
		path := app.db.Path()
		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
		db, err := bolt.Open(path, 0600, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = db.Update(func(tx *bolt.Tx) error {
			for _, record := range []struct{ bucket, key string }{
				{manifestBucket, unrelated.ManifestDigest}, {runBucket, receipt.AddedRuns[0]},
			} {
				bucket, key := tx.Bucket([]byte(record.bucket)), []byte(record.key)
				if restore {
					if err := bucket.Put(key, original[record.bucket]); err != nil {
						return err
					}
				} else {
					original[record.bucket] = bytes.Clone(bucket.Get(key))
					if original[record.bucket] == nil {
						return errors.New("public fixture did not persist the expected primary")
					}
					if err := bucket.Put(key, []byte("{")); err != nil {
						return err
					}
				}
			}
			return nil
		})
		closeErr := db.Close()
		if err != nil || closeErr != nil {
			t.Fatal(errors.Join(err, closeErr))
		}
		// Reopen the actual native handle without running Board startup validation:
		// the public threshold operation itself must select the corruption error.
		app.db, err = bolt.Open(path, 0600, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	editClosedStore(false)
	changes, err := app.ReviewThresholds(draftAuthor, secondProject, draft.ID)
	if !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "invalid storage record in "+manifestBucket) || changes != nil {
		t.Fatalf("catalog corruption lost precedence to Run validation or leaked partial changes: %+v %v", changes, err)
	}
	if held, err := app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
		t.Fatalf("corrupt threshold evidence did not hold assessments: %t %v", held, err)
	}
	editClosedStore(true)
	if err := app.Revalidate(draftAuthor, secondProject); err != nil {
		t.Fatal(err)
	}
	want := []ThresholdChange{{CandidateID: prior.ID, BehaviorID: "BH", Before: 10000, After: 5000, ReducingActors: []string{secondActor}}}
	if changes, err := app.ReviewThresholds(draftAuthor, secondProject, draft.ID); err != nil || !reflect.DeepEqual(changes, want) {
		t.Fatalf("restored evidence did not yield exactly one original reduction: %+v %v", changes, err)
	}
	if approval, err := app.Approve(secondActor, secondProject, draft.ID, draft.Revision); !errors.Is(err, ErrForbidden) || approval.Descriptor.Digest != "" {
		t.Fatalf("reducing actor approved their own failure-masking reduction: %+v %v", approval, err)
	}
	if approval, err := app.Approve(draftAuthor, secondProject, draft.ID, draft.Revision); err != nil || !reflect.DeepEqual(approval.Thresholds, want) {
		t.Fatalf("independent approval lost exact reduction history: %+v %v", approval, err)
	}
	if got, err := app.SourceCSV(secondActor, secondProject, receipt.ID); err != nil || !bytes.Equal(got, source) {
		t.Fatalf("threshold review changed the original failure evidence: %v", err)
	}
	if got, data, err := app.ReportHTML(secondActor, secondProject, frozen.ID); err != nil || got != frozen || !bytes.Equal(data, html) {
		t.Fatalf("threshold review changed the frozen blocked assessment: %v", err)
	}
}

func TestTicket80ThresholdReviewSharedWideContract(t *testing.T) {
	app, original, at := reviewBoard(t)
	wide := original.Baseline
	wide.ID = "T80-WIDE"
	wide.Behaviors = slices.Clone(wide.Behaviors)
	for i := 1; i < 500; i++ {
		wide.Behaviors = append(wide.Behaviors, Behavior{
			ID: fmt.Sprintf("BH-%03d", i), Revision: 1, Required: true, Text: "Independent retained behavior",
			Policy: Policy{ID: fmt.Sprintf("P-%03d", i), Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
			Tests:  []TestPlan{{ID: fmt.Sprintf("T-%03d", i), ExpectedOutcome: "Reviewed result", MustPass: true, PlannedAttempts: 1}},
		})
	}
	draft, err := app.CreateDraft(secondActor, secondProject, wide, DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	high, err := app.Approve(secondActor, secondProject, draft.ID, draft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	var want []ThresholdChange
	for i, mark := range strings.Split("abcdef012345", "") {
		candidate := reviewCandidate(t, app, high, mark)
		reviewCommit(t, app, candidate, reviewRows(candidate, mark, fmt.Sprintf("WIDE-%02d", i), Fail, at))
		want = append(want, ThresholdChange{CandidateID: candidate.ID, BehaviorID: "BH", Before: 10000, After: 5000, ReducingActors: []string{secondActor}})
	}
	slices.SortFunc(want, func(a, b ThresholdChange) int { return strings.Compare(a.CandidateID, b.CandidateID) })
	draft, err = app.CreateDraft(secondActor, secondProject, high.Baseline, DraftOptions{FromBaseline: high.Descriptor.Digest})
	if err != nil {
		t.Fatal(err)
	}
	lower := draft.Baseline
	lower.Behaviors = slices.Clone(lower.Behaviors)
	for i := range lower.Behaviors {
		lower.Behaviors[i].Policy.MinPassBasisPoints = 5000
	}
	draft, err = app.SaveDraft(secondActor, secondProject, draft.ID, draft.Revision, lower)
	if err != nil {
		t.Fatal(err)
	}
	// Bounded engineering comparison only: this is not SPEC-10 host evidence,
	// a capacity trial, or a timing/coverage waiver. Keep all three observations.
	for sample := 0; sample < 3; sample++ {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		changes, err := app.ReviewThresholds(secondActor, secondProject, draft.ID)
		elapsed := time.Since(start)
		runtime.ReadMemStats(&after)
		if err != nil || !reflect.DeepEqual(changes, want) {
			t.Fatalf("shared 500-behavior contract lost exact twelve-candidate reductions/order: %+v %v", changes, err)
		}
		t.Logf("T80_THRESHOLD_COST sample=%d candidates=12 behaviors=500 elapsed_ns=%d allocated_bytes=%d allocations=%d", sample, elapsed.Nanoseconds(), after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs)
	}
}

func TestTicket80ThresholdReviewKeepsHotAndColdApprovalPoliciesSeparate(t *testing.T) {
	app, hot, at := reviewBoard(t)
	coldInput := hot.Baseline
	coldInput.ID = "T80-COLD"
	coldInput.Behaviors = slices.Clone(coldInput.Behaviors)
	coldInput.Behaviors[0].Policy.ID = "P-COLD"
	coldInput.Behaviors[0].Policy.MinPassBasisPoints = 7500
	coldDraft, err := app.CreateDraft(secondActor, secondProject, coldInput, DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Establish this independent contract before either policy has a failed run.
	cold, err := app.Approve(secondActor, secondProject, coldDraft.ID, coldDraft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	var want []ThresholdChange
	for i, input := range []struct {
		mark     string
		approved ApprovedBaseline
		before   int
	}{{"a", hot, 10000}, {"b", hot, 10000}, {"c", cold, 7500}} {
		candidate := reviewCandidate(t, app, input.approved, input.mark)
		rows := reviewRows(candidate, input.mark, fmt.Sprintf("MIXED-%d", i), Fail, at)
		if input.mark == "c" {
			rows = strings.ReplaceAll(rows, ",BH,1,P1,1,", ",BH,1,P-COLD,1,")
		}
		reviewCommit(t, app, candidate, rows)
		want = append(want, ThresholdChange{CandidateID: candidate.ID, BehaviorID: "BH", Before: input.before, After: 5000, ReducingActors: []string{secondActor}})
	}
	slices.SortFunc(want, func(a, b ThresholdChange) int { return strings.Compare(a.CandidateID, b.CandidateID) })
	draft, err := app.CreateDraft(secondActor, secondProject, hot.Baseline, DraftOptions{FromBaseline: hot.Descriptor.Digest})
	if err != nil {
		t.Fatal(err)
	}
	lower := draft.Baseline
	lower.Behaviors = slices.Clone(lower.Behaviors)
	lower.Behaviors[0].Policy.MinPassBasisPoints = 5000
	draft, err = app.SaveDraft(secondActor, secondProject, draft.ID, draft.Revision, lower)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := app.ReviewThresholds(secondActor, secondProject, draft.ID); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("hot/cold approval reuse mixed policies, omitted a reduction or reordered candidates: %+v %v", got, err)
	}
}
