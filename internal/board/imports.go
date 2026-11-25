package board

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"time"

	"release-evidence-board/internal/recordset"

	bolt "go.etcd.io/bbolt"
)

type Preview struct {
	Token, Hash, Schema, OpenCandidateID string
	Revision                             uint64
	Rows                                 int
	Issues                               []Diagnostic
	AddedRuns, ExistingRuns              []string
	HistoricalCandidates                 []string
	Projected                            []Assessment
	Ordering                             map[string]map[string]ImportOrdering
	ExistingReceiptID                    string
}

type ImportOrdering struct {
	PreviousRunID, ProjectedRunID string
	Incoming                      []RunAssessment
}

type Receipt struct {
	ID, Hash, Schema, Uploader, OpenCandidateID string
	ImportedAt                                  time.Time
	Rows                                        int
	LedgerHighWater                             uint64
	AddedRuns, ExistingRuns                     []string
	CandidateIDs, HistoricalCandidates          []string
	PreviousReceipts                            []string
}

type previewBinding struct {
	Actor, CandidateID, Hash string
	Revision                 uint64
	CreatedAt                time.Time
}

type importPlan struct {
	newRuns         []Run
	existing        []Run
	candidates      []string
	issues          []Diagnostic
	ledgerHighWater uint64
}

var errReplayReceipt = errors.New("return the already committed receipt without writing")

func (b *Board) PreviewImport(actor, project, openCandidateID string, raw []byte) (Preview, error) {
	now := b.now()
	preview := Preview{Schema: ResultSchema, OpenCandidateID: openCandidateID}
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, contribute); err != nil {
			return err
		}
		if err := b.assessmentAvailable(tx); err != nil {
			return err
		}
		if _, err := getJSON[Candidate](tx, candidateBucket, openCandidateID); err != nil {
			return err
		}
		preview.Hash = digest(raw)
		var err error
		preview.Revision, err = counter(tx, "data_revision")
		if err != nil {
			return err
		}
		rows, issues := ParseResultsCSV(raw)
		preview.Rows, preview.Issues = len(rows), issues
		if len(issues) > 0 {
			return nil
		}
		plan, err := buildImportPlan(tx, rows, now, b.product)
		if err != nil {
			return err
		}
		preview.Issues = plan.issues
		if len(plan.issues) > 0 {
			return nil
		}
		for _, run := range plan.newRuns {
			preview.AddedRuns = append(preview.AddedRuns, run.ID)
		}
		for _, run := range plan.existing {
			preview.ExistingRuns = append(preview.ExistingRuns, run.ID)
		}
		preview.HistoricalCandidates = historicalCandidates(plan.candidates, openCandidateID)
		if receipt, _, err := retainedReceipt(tx, preview.Hash); err == nil {
			preview.ExistingReceiptID = receipt.ID
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		preview.Projected, err = projectImport(tx, plan, openCandidateID, now)
		if err == nil {
			preview.Ordering = importOrdering(preview.Projected, plan, now)
		}
		return err
	})
	if err != nil {
		return preview, b.failedChange(actor, "import.preview", b.recordIntegrityIssue(actor, err))
	}
	if len(preview.Issues) > 0 {
		return preview, b.recordImportRejection(actor)
	}
	var tokenBytes [32]byte
	// crypto/rand.Read fills the buffer or terminates the process.
	rand.Read(tokenBytes[:])
	preview.Token = hex.EncodeToString(tokenBytes[:])
	binding := previewBinding{Actor: actor, CandidateID: openCandidateID, Hash: preview.Hash, Revision: preview.Revision, CreatedAt: now}
	err = b.change(actor, "import.preview", func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, contribute); err != nil {
			return err
		}
		if err := b.assessmentAvailable(tx); err != nil {
			return err
		}
		revision, err := counter(tx, "data_revision")
		if err != nil {
			return err
		}
		if revision != binding.Revision {
			return fmt.Errorf("%w: evidence changed during preview", ErrConflict)
		}
		if err := removeAbandonedPreviews(tx, now); err != nil {
			return err
		}
		if err := putJSON(tx, previewBucket, preview.Token, binding); err != nil {
			return err
		}
		return tx.Bucket([]byte(previewCSV)).Put([]byte(preview.Token), raw)
	})
	if err != nil {
		preview.Token = ""
		preview.Projected = nil
		preview.Ordering = nil
	}
	return preview, b.recordIntegrityIssue(actor, err)
}

func importOrdering(assessments []Assessment, plan importPlan, now time.Time) map[string]map[string]ImportOrdering {
	newIDs := make(map[string]bool)
	supplied := make(map[string]bool)
	for _, run := range plan.newRuns {
		newIDs[run.ID], supplied[run.ID] = true, true
	}
	for _, run := range plan.existing {
		supplied[run.ID] = true
	}
	projected := make(map[string]map[string]ImportOrdering, len(assessments))
	for _, assessment := range assessments {
		behaviors := make(map[string]ImportOrdering, len(assessment.Behaviors))
		for _, behavior := range assessment.Behaviors {
			order := ImportOrdering{ProjectedRunID: behavior.CurrentRunID}
			var before []Run
			for _, run := range behavior.Runs {
				if !newIDs[run.ID] {
					before = append(before, run.Run)
				}
				if supplied[run.ID] {
					order.Incoming = append(order.Incoming, run)
				}
			}
			previous, _ := EvaluateBehavior(behavior.Behavior, before, now)
			order.PreviousRunID = previous.CurrentRunID
			behaviors[behavior.ID] = order
		}
		projected[assessment.Candidate.ID] = behaviors
	}
	return projected
}

func projectImport(tx *bolt.Tx, plan importPlan, selected string, now time.Time) ([]Assessment, error) {
	projected := make(map[string][]Run)
	projected[selected] = nil
	for _, id := range plan.candidates {
		projected[id] = nil
	}
	for _, run := range plan.newRuns {
		projected[run.CandidateID] = append(projected[run.CandidateID], run)
	}
	var assessments []Assessment
	for _, id := range slices.Sorted(maps.Keys(projected)) {
		assessment, err := makeAssessment(tx, id, now, projected[id])
		if err != nil {
			return nil, err
		}
		assessment.LedgerHighWater = plan.ledgerHighWater
		assessments = append(assessments, assessment)
	}
	return assessments, nil
}

func (b *Board) CommitImport(actor, project, token string, raw []byte) (Receipt, error) {
	var receipt Receipt
	now := b.now()
	err := b.db.View(func(tx *bolt.Tx) error {
		binding, _, err := b.boundCSV(tx, actor, project, token, raw)
		if err != nil {
			return err
		}
		receipt, _, err = retainedReceipt(tx, binding.Hash)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return receipt, b.commitFailure(actor, err)
	}
	if receipt.ID != "" {
		return receipt, nil
	}
	err = b.db.Update(func(tx *bolt.Tx) error {
		binding, original, err := b.boundCSV(tx, actor, project, token, raw)
		if err != nil {
			return err
		}
		receipt, _, err = retainedReceipt(tx, binding.Hash)
		if err == nil {
			// Another writer committed after the view. Roll back this
			// transaction rather than syncing an empty new meta page.
			return errReplayReceipt
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if err := b.assessmentAvailable(tx); err != nil {
			return err
		}
		revision, err := counter(tx, "data_revision")
		if err != nil {
			return err
		}
		if binding.Revision != revision || !now.Before(binding.CreatedAt.Add(30*time.Minute)) {
			return fmt.Errorf("%w: preview expired or evidence changed", ErrConflict)
		}
		if _, _, _, err := verifiedCandidateRecords(tx, binding.CandidateID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return integrity("saved preview workspace is no longer retained")
			}
			return err
		}
		rows, issues := ParseResultsCSV(original)
		if len(issues) > 0 {
			return invalid("CSV no longer passes validation")
		}
		plan, err := buildImportPlan(tx, rows, now, b.product)
		if err != nil {
			return err
		}
		if len(plan.issues) > 0 {
			return invalid("CSV no longer passes validation")
		}
		receipt, err = b.persistImport(tx, plan, original, binding, actor, now, len(rows))
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(previewCSV)).Delete([]byte(token))
	})
	if errors.Is(err, errReplayReceipt) {
		return receipt, nil
	}
	return receipt, b.commitFailure(actor, err)
}

func (b *Board) boundCSV(tx *bolt.Tx, actor, project, token string, raw []byte) (previewBinding, []byte, error) {
	if err := b.authorize(tx, actor, project, contribute); err != nil {
		return previewBinding{}, nil, err
	}
	binding, err := getJSON[previewBinding](tx, previewBucket, token)
	if err != nil {
		return previewBinding{}, nil, err
	}
	if binding.Actor != actor {
		return previewBinding{}, nil, ErrNotFound
	}
	original := raw
	if original == nil {
		original = tx.Bucket([]byte(previewCSV)).Get([]byte(token))
		if original == nil {
			_, original, err = retainedReceipt(tx, binding.Hash)
			if err != nil {
				return previewBinding{}, nil, err
			}
		}
	}
	if len(original) > MaxCSVBytes || digest(original) != binding.Hash {
		return previewBinding{}, nil, fmt.Errorf("%w: CSV bytes differ from the preview", ErrConflict)
	}
	return binding, original, nil
}

func (b *Board) commitFailure(actor string, err error) error {
	if errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict) {
		return errors.Join(err, b.recordImportRejection(actor))
	}
	return b.failedChange(actor, "import.commit", b.recordIntegrityIssue(actor, err))
}

func (b *Board) persistImport(tx *bolt.Tx, plan importPlan, raw []byte, binding previewBinding, actor string, now time.Time, rowCount int) (Receipt, error) {
	receipt := Receipt{ID: "I-" + binding.Hash, Hash: binding.Hash, Schema: ResultSchema,
		Uploader: actor, OpenCandidateID: binding.CandidateID, ImportedAt: now, Rows: rowCount,
		CandidateIDs: plan.candidates, HistoricalCandidates: historicalCandidates(plan.candidates, binding.CandidateID),
	}
	references := make(map[string]bool)
	for _, run := range plan.existing {
		receipt.ExistingRuns = append(receipt.ExistingRuns, run.ID)
		references[run.ReceiptID] = true
	}
	receipt.PreviousReceipts = slices.Sorted(maps.Keys(references))
	if len(plan.newRuns) != 0 {
		if err := tx.Bucket([]byte(metaBucket)).Put([]byte("ledger"), strconv.AppendUint(nil, plan.ledgerHighWater, 10)); err != nil {
			return Receipt{}, err
		}
	}
	for _, run := range plan.newRuns {
		run.ReceiptID = receipt.ID
		run.FileHash = receipt.Hash
		if err := persistRun(tx, run); err != nil {
			return Receipt{}, err
		}
		receipt.AddedRuns = append(receipt.AddedRuns, run.ID)
	}
	receipt.LedgerHighWater = plan.ledgerHighWater
	if err := tx.Bucket([]byte(csvBucket)).Put([]byte(binding.Hash), raw); err != nil {
		return Receipt{}, err
	}
	if err := putJSON(tx, receiptBucket, binding.Hash, receipt); err != nil {
		return Receipt{}, err
	}
	if _, err := increment(tx, "data_revision"); err != nil {
		return Receipt{}, err
	}
	err := b.audit(tx, actor, "import.commit", "success", receipt.ID)
	return receipt, err
}

func persistRun(tx *bolt.Tx, run Run) error {
	if err := putJSON(tx, runBucket, run.ID, run); err != nil {
		return err
	}
	if err := tx.Bucket([]byte(candidateRun)).Put([]byte(run.CandidateID+"\x00"+run.ID), []byte(run.ID)); err != nil {
		return err
	}
	for _, row := range run.Rows {
		if row.Kind() == Result {
			if err := putJSON(tx, resultBucket, row.ResultID(), run.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func removeAbandonedPreviews(tx *bolt.Tx, now time.Time) error {
	stored := tx.Bucket([]byte(previewBucket))
	uploads := tx.Bucket([]byte(previewCSV))
	var expired []recordset.Key
	err := stored.ForEach(func(key, _ []byte) error {
		binding, err := getJSON[previewBinding](tx, previewBucket, string(key))
		if err != nil {
			return err
		}
		if now.Before(binding.CreatedAt.Add(30 * time.Minute)) {
			return nil
		}
		if err := uploads.Delete(key); err != nil {
			return err
		}
		if tx.Bucket([]byte(receiptBucket)).Get([]byte(binding.Hash)) == nil {
			expired = append(expired, recordset.Key{Bucket: previewBucket, Key: string(key)})
		}
		return nil
	})
	if err != nil {
		return err
	}
	return recordset.Delete(tx, expired...)
}

func (b *Board) CancelPreview(actor, project, token string) error {
	return b.change(actor, "import.cancel", func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, contribute); err != nil {
			return err
		}
		binding, err := getJSON[previewBinding](tx, previewBucket, token)
		if err != nil {
			return err
		}
		if binding.Actor != actor {
			return ErrNotFound
		}
		if tx.Bucket([]byte(receiptBucket)).Get([]byte(binding.Hash)) == nil {
			return recordset.Delete(tx, recordset.Key{Bucket: previewBucket, Key: token}, recordset.Key{Bucket: previewCSV, Key: token})
		}
		return recordset.Delete(tx, recordset.Key{Bucket: previewCSV, Key: token})
	})
}

func (b *Board) recordImportRejection(actor string) error {
	return b.db.Update(func(tx *bolt.Tx) error {
		if _, err := increment(tx, "import_rejections"); err != nil {
			return err
		}
		return b.audit(tx, actor, "import.reject", "failure")
	})
}

func historicalCandidates(ids []string, selected string) []string {
	var historical []string
	for _, id := range ids {
		if id != selected {
			historical = append(historical, id)
		}
	}
	return historical
}

type rosterReference struct {
	release string
	entries map[string]behaviorReference
}

type behaviorReference struct {
	behavior Behavior
	tests    map[string]int
}

func importRoster(tx *bolt.Tx, id string) (rosterReference, error) {
	_, manifest, baseline, err := verifiedCandidateRecords(tx, id)
	if err != nil {
		return rosterReference{}, err
	}
	reference := rosterReference{release: manifest.Manifest.ReleaseID, entries: make(map[string]behaviorReference)}
	for _, behavior := range baseline.Baseline.Behaviors {
		tests := make(map[string]int)
		for _, test := range behavior.Tests {
			tests[test.ID] = test.PlannedAttempts
		}
		reference.entries[behavior.ID] = behaviorReference{behavior: behavior, tests: tests}
	}
	return reference, nil
}

func buildImportPlan(tx *bolt.Tx, rows []SourceRow, now time.Time, product string) (importPlan, error) {
	plan := importPlan{}
	runs := make(map[string]Run)
	resultIDs := make(map[string]bool)
	seenSlots := make(map[string]bool)
	rosters := make(map[string]rosterReference)
	for _, row := range rows {
		fields := row.Fields
		if fields[colProduct] != product {
			plan.issues = append(plan.issues, diagnostic(row, colProduct, "PRODUCT", "Use the authorized project product"))
			continue
		}
		candidateID := "C-" + digest([]byte(fields[colManifest]+fields[colBaseline]))
		roster, loaded := rosters[candidateID]
		if !loaded {
			var err error
			roster, err = importRoster(tx, candidateID)
			if errors.Is(err, ErrNotFound) {
				plan.issues = append(plan.issues, diagnostic(row, colManifest, "UNKNOWN_PAIR", "Create the exact manifest and approved baseline candidate pair before import"))
				continue
			}
			if err != nil {
				return plan, err
			}
			rosters[candidateID] = roster
		}
		issue := validateRowReference(row, roster, now)
		if issue != nil {
			plan.issues = append(plan.issues, *issue)
			continue
		}
		run, seen := runs[row.RunID()]
		closed, _ := parseTimestamp(row.ClosedAt())
		if !seen {
			run = Run{ID: row.RunID(), CandidateID: candidateID, BehaviorID: fields[colBehavior], Closed: closed, ImportedAt: now}
		} else if issue := compareRunRow(row, run, candidateID, closed); issue != nil {
			plan.issues = append(plan.issues, *issue)
			continue
		}
		if row.Kind() == Result {
			key := run.ID + ":" + row.TestID() + ":" + row.Attempt()
			if resultIDs[row.ResultID()] || seenSlots[key] {
				plan.issues = append(plan.issues, diagnostic(row, colResult, "DUPLICATE_SLOT", "A file cannot repeat a result ID or a test attempt in a run"))
				continue
			}
			resultIDs[row.ResultID()] = true
			seenSlots[key] = true
		}
		run.Rows = append(run.Rows, row)
		runs[run.ID] = run
	}
	if len(plan.issues) != 0 {
		return plan, nil
	}
	plan.candidates = slices.Sorted(maps.Keys(rosters))
	sequence, err := counter(tx, "ledger")
	if err != nil {
		return plan, err
	}
	if err := checkLedgerWitnesses(tx, sequence); err != nil {
		return plan, err
	}
	checkedReceipts := make(map[string]bool)
	for _, id := range slices.Sorted(maps.Keys(runs)) {
		run := runs[id]
		existing, err := getJSON[Run](tx, runBucket, id)
		if err == nil {
			if !checkedReceipts[existing.FileHash] {
				if _, _, err := retainedReceipt(tx, existing.FileHash); err != nil {
					return plan, integrity("an existing run lacks its original receipt")
				}
				checkedReceipts[existing.FileHash] = true
			}
			if !identicalRows(existing.Rows, run.Rows) {
				plan.issues = append(plan.issues, diagnostic(run.Rows[0], colRun, "IMMUTABLE_RUN", "The run exists with a different row set. Use a new run ID for a new execution"))
				continue
			}
			plan.existing = append(plan.existing, existing)
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			return plan, err
		}
		if tx.Bucket([]byte(purgedRunBucket)).Get([]byte(id)) != nil {
			plan.issues = append(plan.issues, diagnostic(run.Rows[0], colRun, "PURGED_RUN", "This run ID was previously used. Use a new run ID"))
			continue
		}
		for _, row := range run.Rows {
			if row.Kind() == Result && tx.Bucket([]byte(resultBucket)).Get([]byte(row.ResultID())) != nil {
				plan.issues = append(plan.issues, diagnostic(row, colResult, "RESULT_EXISTS", "Result IDs must be unique across the product"))
			}
		}
		if sequence == math.MaxUint64 {
			return plan, invalid("evidence ledger capacity is exhausted")
		}
		sequence++
		run.Sequence = sequence
		plan.newRuns = append(plan.newRuns, run)
	}
	plan.ledgerHighWater = sequence
	return plan, nil
}

func validateRowReference(row SourceRow, roster rosterReference, now time.Time) *Diagnostic {
	fields := row.Fields
	var col int
	var code, reason string
	behavior, found := roster.entries[fields[colBehavior]]
	closed, _ := parseTimestamp(row.ClosedAt())
	switch {
	case roster.release != fields[colRelease]:
		col, code, reason = colRelease, "RELEASE_MISMATCH", "Release ID must match the stored manifest"
	case !found:
		col, code, reason = colBehavior, "UNKNOWN_BEHAVIOR", "Use an approved behavior in the exact baseline"
	case fields[colBehaviorRev] != strconv.Itoa(behavior.behavior.Revision):
		col, code, reason = colBehaviorRev, "BEHAVIOR_REVISION", "Behavior revision must match the exact approved baseline"
	case fields[colPolicy] != behavior.behavior.Policy.ID:
		col, code, reason = colPolicy, "POLICY_ID", "Policy must match the exact approved baseline"
	case fields[colPolicyRev] != strconv.Itoa(behavior.behavior.Policy.Revision):
		col, code, reason = colPolicyRev, "POLICY_REVISION", "Policy revision must match the exact approved baseline"
	case closed.After(now.Add(5 * time.Minute)):
		col, code, reason = colRunFinished, "CLOCK_SKEW_LIMIT", "Run finish cannot be over 300 seconds ahead of import time"
	case row.Kind() == Result:
		capacity, exists := behavior.tests[row.TestID()]
		number, _ := positiveDecimal(row.Attempt())
		switch {
		case !exists:
			col, code, reason = colTest, "UNKNOWN_TEST", "Use a test planned for this behavior"
		case number > capacity:
			col, code, reason = colAttempt, "UNPLANNED_ATTEMPT", "Attempt index exceeds the approved plan"
		}
	}
	if code == "" {
		return nil
	}
	issue := diagnostic(row, col, code, reason)
	return &issue
}

func compareRunRow(row SourceRow, run Run, candidateID string, closed time.Time) *Diagnostic {
	var issue Diagnostic
	switch {
	case run.CandidateID != candidateID || run.BehaviorID != row.Fields[colBehavior]:
		issue = diagnostic(row, colRun, "RUN_IDENTITY", "One run must use one candidate and behavior")
	case !run.Closed.Equal(closed):
		issue = diagnostic(row, colRunFinished, "RUN_FINISH_MISMATCH", "Every row for the run must share the same finish instant")
	case row.Kind() == RunOnly || run.Rows[0].Kind() == RunOnly:
		issue = diagnostic(row, colType, "RUN_ONLY_MIXED", "RUN_ONLY must be the sole row for its run")
	default:
		return nil
	}
	return &issue
}

func identicalRows(old, next []SourceRow) bool {
	if len(old) != len(next) {
		return false
	}
	members := make(map[[columnCount]string]bool, len(old))
	for _, row := range old {
		members[row.Fields] = true
	}
	for _, row := range next {
		if !members[row.Fields] {
			return false
		}
	}
	return true
}

func (b *Board) Receipt(actor, project, id string) (Receipt, error) {
	var receipt Receipt
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, read); err != nil {
			return err
		}
		var err error
		receipt, _, err = retainedReceipt(tx, receiptHash(id))
		return err
	})
	return receipt, b.recordIntegrityIssue(actor, err)
}

func receiptHash(id string) string {
	if len(id) == 66 && id[:2] == "I-" && digestPattern.MatchString(id[2:]) {
		return id[2:]
	}
	return "-"
}

func (b *Board) SourceCSV(actor, project, receiptID string) ([]byte, error) {
	var original []byte
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorize(tx, actor, project, read); err != nil {
			return err
		}
		_, raw, err := retainedReceipt(tx, receiptHash(receiptID))
		if err != nil {
			return err
		}
		original = slices.Clone(raw)
		return nil
	})
	return original, b.recordIntegrityIssue(actor, err)
}
