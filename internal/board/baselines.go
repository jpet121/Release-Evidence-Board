package board

import (
	"bytes"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"time"

	"release-evidence-board/internal/recordset"

	bolt "go.etcd.io/bbolt"
)

type DraftOptions struct {
	FromBaseline string
	FromDraft    string
}

type Draft struct {
	ID, Author, ParentBaseline, ParentDraft, CopyActor string
	Revision                                           uint64
	Baseline                                           Baseline
	ApprovedDigest                                     string
	ThresholdEdits                                     []ThresholdEdit
	DerivedBehaviorIDs                                 []string
}

type ThresholdEdit struct {
	BehaviorID, Actor string
	Before, After     int
	Initial           bool
}

func (d Draft) InheritedBehavior(id string) bool {
	return slices.Contains(d.DerivedBehaviorIDs, id)
}

type ThresholdChange struct {
	CandidateID, BehaviorID string
	Before, After           int
	ReducingActors          []string
}

type ApprovedBaseline struct {
	Baseline     Baseline
	Descriptor   Descriptor
	DraftID      string
	Author       string
	Approver     string
	ApprovedAt   time.Time
	SelfApproval bool
	Thresholds   []ThresholdChange
}

func showableDraft(contents Baseline) error {
	if !idPattern.MatchString(contents.ID) || contents.Revision < 0 || len(contents.Behaviors) > MaxBehaviors {
		return invalid("draft requires a valid baseline ID, positive revision and at most 500 behaviors")
	}
	tests := 0
	for _, behavior := range contents.Behaviors {
		tests += len(behavior.Tests)
	}
	if tests > MaxAttempts {
		return invalid("draft exceeds 10,000 planned tests")
	}
	return nil
}

func (b *Board) CreateDraft(actor, project string, contents Baseline, options DraftOptions) (Draft, error) {
	var draft Draft
	err := b.change(actor, "draft.create", func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, contribute); err != nil {
			return err
		}
		if options.FromBaseline != "" && options.FromDraft != "" {
			return invalid("select one draft or baseline to derive from")
		}
		if contents.ProductID != b.product {
			return invalid("draft product does not match the project")
		}
		if err := showableDraft(contents); err != nil {
			return err
		}
		draft.Author = actor
		draft.ParentBaseline = options.FromBaseline
		draft.ParentDraft = options.FromDraft
		var previous Baseline
		if options.FromBaseline != "" {
			parent, err := getApprovedBaseline(tx, options.FromBaseline)
			if err != nil {
				return err
			}
			previous = parent.Baseline
			draft.DerivedBehaviorIDs = slices.Sorted(maps.Keys(behaviorsByID(parent.Baseline)))
			if previous.ID == contents.ID && contents.Revision <= previous.Revision {
				contents.Revision = previous.Revision + 1
			}
		}
		if options.FromDraft != "" {
			parent, err := getRetainedDraft(tx, options.FromDraft)
			if err != nil {
				return err
			}
			previous = parent.Baseline
			draft.ParentBaseline = parent.ParentBaseline
			draft.CopyActor = actor
			draft.ThresholdEdits = slices.Clone(parent.ThresholdEdits)
			draft.DerivedBehaviorIDs = slices.Clone(parent.DerivedBehaviorIDs)
			if contents.ID == previous.ID && contents.Revision <= previous.Revision {
				contents.Revision = previous.Revision + 1
			}
		}
		if contents.Revision == 0 {
			contents.Revision = 1
		}
		if err := rejectDerivedRekey(draft.DerivedBehaviorIDs, previous, contents); err != nil {
			return err
		}
		draft.Baseline = reviseBehaviors(contents, previous)
		draft.ThresholdEdits = append(draft.ThresholdEdits, reductions(actor, previous, draft.Baseline)...)
		records := tx.Bucket([]byte(draftBucket))
		sequence, err := recordset.AdvanceSequence(records, invalid("draft identifier capacity is exhausted"))
		if err != nil {
			return err
		}
		draft.ID = "D-" + strconv.FormatUint(sequence, 10)
		draft.Revision = 1
		if tx.Bucket([]byte(draftBucket)).Get([]byte(draft.ID)) != nil || tx.Bucket([]byte(draftMetaBucket)).Get([]byte(draft.ID)) != nil {
			return integrity("draft sequence already exists")
		}
		return b.commitDraft(tx, actor, "draft.create", draft)
	})
	return draft, b.recordIntegrityIssue(actor, err)
}

func reviseBehaviors(contents, previous Baseline) Baseline {
	contents.Behaviors = slices.Clone(contents.Behaviors)
	earlier := behaviorsByID(previous)
	for i, behavior := range contents.Behaviors {
		old, found := earlier[behavior.ID]
		if !found {
			continue
		}
		if behavior.Revision > 0 && behavior.Revision <= old.Revision && (behavior.Required != old.Required || behavior.Text != old.Text || !sameTests(behavior.Tests, old.Tests)) {
			behavior.Revision = old.Revision + 1
		}
		if behavior.Policy.ID == old.Policy.ID && behavior.Policy.Revision > 0 && behavior.Policy.Revision <= old.Policy.Revision &&
			(behavior.Policy.MinPassBasisPoints != old.Policy.MinPassBasisPoints || behavior.Policy.MaxAgeHours != old.Policy.MaxAgeHours) {
			behavior.Policy.Revision = old.Policy.Revision + 1
		}
		contents.Behaviors[i] = behavior
	}
	return contents
}

func sameTests(a, b []TestPlan) bool {
	if len(a) != len(b) {
		return false
	}
	left, right := slices.Clone(a), slices.Clone(b)
	slices.SortFunc(left, compareTests)
	slices.SortFunc(right, compareTests)
	return slices.Equal(left, right)
}

func compareTests(a, b TestPlan) int {
	if a.ID < b.ID {
		return -1
	}
	if a.ID > b.ID {
		return 1
	}
	return 0
}

func behaviorsByID(b Baseline) map[string]Behavior {
	index := make(map[string]Behavior, len(b.Behaviors))
	for _, behavior := range b.Behaviors {
		index[behavior.ID] = behavior
	}
	return index
}

func reductions(actor string, previous, current Baseline) []ThresholdEdit {
	earlier := behaviorsByID(previous)
	var added []ThresholdEdit
	for _, behavior := range current.Behaviors {
		old, found := earlier[behavior.ID]
		previousMark := old.Policy.MinPassBasisPoints
		mark := behavior.Policy.MinPassBasisPoints
		if !found || mark < previousMark {
			added = append(added, ThresholdEdit{BehaviorID: behavior.ID, Actor: actor, Before: previousMark, After: mark, Initial: !found})
		}
	}
	return added
}

// A save can explicitly add or remove a behavior. Replacing an inherited ID
// in the same edit would discard the origin needed for threshold review.
func rejectDerivedRekey(inherited []string, previous, current Baseline) error {
	old := behaviorsByID(previous)
	next := behaviorsByID(current)
	added := false
	for id := range next {
		if _, found := old[id]; !found {
			added = true
			break
		}
	}
	if !added {
		return nil
	}
	for _, id := range inherited {
		if _, wasPresent := old[id]; wasPresent {
			if _, stillPresent := next[id]; !stillPresent {
				return invalid("keep the ID of an inherited behavior when editing its contract")
			}
		}
	}
	return nil
}

func (b *Board) SaveDraft(actor, project, id string, revision uint64, contents Baseline) (Draft, error) {
	var draft Draft
	err := b.change(actor, "draft.edit", func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, contribute); err != nil {
			return err
		}
		var err error
		draft, err = getRetainedDraft(tx, id)
		if err != nil {
			return err
		}
		if draft.Revision != revision || draft.ApprovedDigest != "" {
			return ErrConflict
		}
		if draft.Revision == math.MaxUint64 {
			return invalid("draft revision capacity is exhausted")
		}
		if contents.ProductID != b.product || contents.ID != draft.Baseline.ID || contents.Revision != draft.Baseline.Revision {
			return invalid("draft identity is immutable")
		}
		if err := showableDraft(contents); err != nil {
			return err
		}
		draft.ThresholdEdits = append(draft.ThresholdEdits, reductions(actor, draft.Baseline, contents)...)
		if err := rejectDerivedRekey(draft.DerivedBehaviorIDs, draft.Baseline, contents); err != nil {
			return err
		}
		draft.Baseline = reviseBehaviors(contents, draft.Baseline)
		draft.Revision++
		return b.commitDraft(tx, actor, "draft.edit", draft)
	})
	return draft, b.recordIntegrityIssue(actor, err)
}

func (b *Board) commitDraft(tx *bolt.Tx, actor, action string, draft Draft) error {
	if err := putBoundJSON(tx, draftBucket, draftMetaBucket, draft.ID, draft); err != nil {
		return err
	}
	return b.audit(tx, actor, action, "success", draft.ID)
}

func (b *Board) Approve(actor, project, id string, revision uint64) (ApprovedBaseline, error) {
	var approved ApprovedBaseline
	err := b.change(actor, "baseline.approve", func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, review); err != nil {
			return err
		}
		if err := b.assessmentAvailable(tx); err != nil {
			return err
		}
		draft, err := getRetainedDraft(tx, id)
		if err != nil {
			return err
		}
		if draft.Revision != revision || draft.ApprovedDigest != "" {
			return ErrConflict
		}
		descriptor, err := draft.Baseline.Descriptor()
		if err != nil {
			return invalid(err.Error())
		}
		changes, err := thresholdChanges(tx, draft)
		if err != nil {
			return err
		}
		for _, change := range changes {
			if slices.Contains(change.ReducingActors, actor) {
				return fmt.Errorf("%w: a different reviewer must approve the lower pass mark for %s", ErrForbidden, change.BehaviorID)
			}
		}
		if err := registerApprovedRevisions(tx, draft.Baseline, descriptor.Digest); err != nil {
			return err
		}
		approved = ApprovedBaseline{Baseline: draft.Baseline, Descriptor: descriptor, DraftID: id,
			Author: draft.Author, Approver: actor, ApprovedAt: b.now(), SelfApproval: draft.Author == actor, Thresholds: changes}
		draft.ApprovedDigest = descriptor.Digest
		approvedBinding := recordset.Key{Bucket: baselineMetaBucket, Key: descriptor.Digest}
		draftBinding := recordset.Key{Bucket: draftMetaBucket, Key: id}
		if err := recordset.WriteJSON(tx,
			recordset.JSONRecord{Key: recordset.Key{Bucket: baselineBucket, Key: descriptor.Digest}, Value: approved, Binding: &approvedBinding},
			recordset.JSONRecord{Key: recordset.Key{Bucket: draftBucket, Key: id}, Value: draft, Binding: &draftBinding}); err != nil {
			return err
		}
		if _, err := increment(tx, "data_revision"); err != nil {
			return err
		}
		return b.audit(tx, actor, "baseline.approve", "success", id, draft.Author, descriptor.Digest)
	})
	return approved, b.recordIntegrityIssue(actor, err)
}

func (b *Board) ReviewThresholds(actor, project, id string) ([]ThresholdChange, error) {
	var result []ThresholdChange
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, contribute); err != nil {
			return err
		}
		draft, err := getRetainedDraft(tx, id)
		if err != nil {
			return err
		}
		result, err = thresholdChanges(tx, draft)
		return err
	})
	return result, b.recordIntegrityIssue(actor, err)
}

// These owned projections live only in the transaction that verified them.
// Retain only candidate identities and one most-reused approved contract, not
// a roster or replacement cross-product for every distinct approval. A cold
// contract is read and released for its current candidate in the same snapshot.
// No projection is used before the full catalog succeeds, so unrelated
// descriptor corruption retains priority over Run errors.
type thresholdContract struct {
	previous map[string]Behavior
	falling  map[string][]Behavior
}

type thresholdCandidate struct {
	id, baselineDigest string
}

func thresholdChanges(tx *bolt.Tx, draft Draft) ([]ThresholdChange, error) {
	next := behaviorsByID(draft.Baseline)
	var candidates []thresholdCandidate
	counts := make(map[string]int)
	var shared Baseline
	var sharedDigest string
	most := 0
	err := visitDescriptorCatalog(tx, func(candidate Candidate, _ StoredManifest, approved ApprovedBaseline) {
		key := approved.Descriptor.Digest
		candidates = append(candidates, thresholdCandidate{id: candidate.ID, baselineDigest: key})
		counts[key]++
		if counts[key] > most {
			shared, sharedDigest, most = approved.Baseline, key, counts[key]
		}
	})
	if err != nil {
		return nil, err
	}
	var changes []ThresholdChange
	work := thresholdWork{draft: draft, next: next, sharedDigest: sharedDigest, sharedBaseline: shared}
	shared = Baseline{}
	for _, candidate := range candidates {
		changes, err = work.appendCandidateChanges(tx, candidate, changes)
		if err != nil {
			return nil, err
		}
	}
	return changes, nil
}

// thresholdWork owns at most one reusable hot contract and the current cold
// lookup. The full catalog must be verified before its first candidate call.
// Appending to the caller's accumulator retains original order without a
// second allocation of the candidate's complete reduction result.
type thresholdWork struct {
	draft          Draft
	next           map[string]Behavior
	sharedDigest   string
	sharedBaseline Baseline
	shared         thresholdContract
}

func (work *thresholdWork) appendCandidateChanges(tx *bolt.Tx, candidate thresholdCandidate, changes []ThresholdChange) ([]ThresholdChange, error) {
	var contract thresholdContract
	if candidate.baselineDigest == work.sharedDigest {
		if work.shared.previous == nil {
			work.shared = fallingThresholds(work.next, work.draft.Baseline, work.sharedBaseline)
			work.sharedBaseline = Baseline{}
		}
		contract = work.shared
	} else {
		approved, err := getApprovedBaseline(tx, candidate.baselineDigest)
		if err != nil {
			return nil, err
		}
		contract = fallingThresholds(work.next, work.draft.Baseline, approved.Baseline)
	}
	if len(contract.falling) == 0 {
		return changes, nil
	}
	runs, _, err := verifiedCandidateRuns(tx, candidate.id)
	if err != nil {
		return nil, err
	}
	// A contract is shared, but each candidate independently contributes
	// at most one change for each definitively failed behavior.
	handled := make(map[string]bool)
	for _, run := range runs {
		updates, found := contract.falling[run.BehaviorID]
		previous := contract.previous[run.BehaviorID]
		if !found || handled[run.BehaviorID] || !evaluateRun(previous, run).Definitive {
			continue
		}
		handled[run.BehaviorID] = true
		for _, updated := range updates {
			changes = append(changes, thresholdChange(candidate.id, previous, updated, work.draft))
		}
	}
	return changes, nil
}

func fallingThresholds(next map[string]Behavior, draft, approved Baseline) thresholdContract {
	falling := make(map[string][]Behavior)
	previousIDs := behaviorsByID(approved)
	for _, previous := range approved.Behaviors {
		updated, found := next[previous.ID]
		if found {
			if updated.Policy.MinPassBasisPoints < previous.Policy.MinPassBasisPoints {
				falling[previous.ID] = []Behavior{updated}
			}
		}
		// An optionalized inherited behavior no longer gates the candidate.
		// Its exact required replacement still carries the higher failed
		// threshold into independent review.
		optionalized := found && previous.Required && !updated.Required
		if found && !optionalized {
			continue
		}
		for _, renamed := range draft.Behaviors {
			if _, wasKnown := previousIDs[renamed.ID]; !wasKnown &&
				(!optionalized || renamed.Required) &&
				renamed.Policy.MinPassBasisPoints < previous.Policy.MinPassBasisPoints && sameEvidenceIntent(previous, renamed) {
				falling[previous.ID] = append(falling[previous.ID], renamed)
			}
		}
	}
	previous := make(map[string]Behavior, len(falling))
	for id := range falling {
		previous[id] = previousIDs[id]
	}
	return thresholdContract{previous: previous, falling: falling}
}

func thresholdChange(candidateID string, previous, updated Behavior, draft Draft) ThresholdChange {
	previousMark := previous.Policy.MinPassBasisPoints
	reducing := make(map[string]bool)
	for _, edit := range draft.ThresholdEdits {
		if (edit.BehaviorID == previous.ID || edit.BehaviorID == updated.ID) && edit.After < previousMark {
			reducing[edit.Actor] = true
		}
	}
	if len(reducing) == 0 {
		reducing[draft.Author] = true
	}
	return ThresholdChange{CandidateID: candidateID, BehaviorID: previous.ID,
		Before: previousMark, After: updated.Policy.MinPassBasisPoints, ReducingActors: slices.Sorted(maps.Keys(reducing))}
}

func sameEvidenceIntent(previous, next Behavior) bool {
	if previous.Text != next.Text || len(previous.Tests) != len(next.Tests) {
		return false
	}
	counts := make(map[TestPlan]int, len(previous.Tests))
	for _, test := range previous.Tests {
		test.ID = ""
		counts[test]++
	}
	for _, test := range next.Tests {
		test.ID = ""
		if counts[test] == 0 {
			return false
		}
		counts[test]--
	}
	return true
}

func registerApprovedRevisions(tx *bolt.Tx, baseline Baseline, baselineDigest string) error {
	key := versionKey(baseline.ID, baseline.Revision)
	if tx.Bucket([]byte(baselineRevision)).Get([]byte(key)) != nil {
		return fmt.Errorf("%w: baseline ID and revision were already approved", ErrConflict)
	}
	if err := putJSON(tx, baselineRevision, key, baselineDigest); err != nil {
		return err
	}
	return visitBaselineIdentities(baseline, func(bucket, key string, value any) error {
		return registerImmutable(tx, bucket, key, value)
	})
}

func visitBaselineIdentities(baseline Baseline, visit func(bucket, key string, value any) error) error {
	for _, behavior := range baseline.Behaviors {
		roster := slices.Clone(behavior.Tests)
		slices.SortFunc(roster, compareTests)
		content := struct {
			Text     string
			Required bool
			Tests    []TestPlan
		}{behavior.Text, behavior.Required, roster}
		if err := visit(behaviorRevision, versionKey(behavior.ID, behavior.Revision), content); err != nil {
			return err
		}
		if err := visit(policyRevision, versionKey(behavior.Policy.ID, behavior.Policy.Revision), behavior.Policy); err != nil {
			return err
		}
		for _, test := range behavior.Tests {
			if err := visit(testOwners, test.ID, behavior.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func versionKey(id string, revision int) string { return id + ":" + strconv.Itoa(revision) }

func immutableFingerprint(value any) ([]byte, error) {
	_, binding, err := recordset.EncodeJSON(value)
	return binding, err
}

func registerImmutable(tx *bolt.Tx, bucket, key string, value any) error {
	fingerprint, err := immutableFingerprint(value)
	if err != nil {
		return err
	}
	records := tx.Bucket([]byte(bucket))
	existing := records.Get([]byte(key))
	if existing == nil {
		return records.Put([]byte(key), fingerprint)
	}
	if !bytes.Equal(existing, fingerprint) {
		return fmt.Errorf("%w: an approved identity cannot be repurposed (%s)", ErrConflict, key)
	}
	return nil
}

func (b *Board) Baseline(actor, project, digest string) (ApprovedBaseline, error) {
	var approved ApprovedBaseline
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, read); err != nil {
			return err
		}
		var err error
		approved, err = getApprovedBaseline(tx, digest)
		return err
	})
	return approved, b.recordIntegrityIssue(actor, err)
}

func (b *Board) Draft(actor, project, id string) (Draft, error) {
	var draft Draft
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, read); err != nil {
			return err
		}
		var err error
		draft, err = getRetainedDraft(tx, id)
		return err
	})
	return draft, b.recordIntegrityIssue(actor, err)
}

func (b *Board) Baselines(actor, project string) ([]ApprovedBaseline, []Draft, error) {
	var approved []ApprovedBaseline
	var drafts []Draft
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, read); err != nil {
			return err
		}
		err := visitApprovedBaselines(tx, b.product, func(item ApprovedBaseline) error {
			approved = append(approved, item)
			return nil
		})
		if err != nil {
			return err
		}
		return visitRetainedDrafts(tx, b.product, func(draft Draft) error {
			drafts = append(drafts, draft)
			return nil
		})
	})
	return approved, drafts, b.recordIntegrityIssue(actor, err)
}
