package board

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"release-evidence-board/internal/recordset"

	bolt "go.etcd.io/bbolt"
)

type PurgePlan struct {
	CandidateIDs, Ineligible, SharedBlockers []string
	ReceiptIDs, ReportIDs, Runs              []string
	OrphanBaselines, OrphanManifests         []string
	AuditSequences                           []uint64
	Revision                                 uint64
	Confirmation                             string
	ExternalCopiesNotPurged                  bool
	AIRequestIDs                             []string `json:",omitempty"`
}

// purgeSelection stays inside the transaction that verified these identities.
// Preview exposes only plan; commit always builds a fresh selection.
type purgeSelection struct {
	plan               PurgePlan
	candidateMaterials map[string]string
	runCandidates      map[string]string
}

func (b *Board) PreviewPurge(actor, project string, ids []string) (PurgePlan, error) {
	var plan PurgePlan
	err := b.db.View(func(tx *bolt.Tx) error {
		if err := b.authorizeCustody(tx, actor, project); err != nil {
			return err
		}
		selection, err := planPurge(tx, ids, b.now())
		plan = selection.plan
		return err
	})
	return plan, b.recordIntegrityIssue(actor, err)
}

func planPurge(tx *bolt.Tx, ids []string, now time.Time) (purgeSelection, error) {
	selection := purgeSelection{
		plan:               PurgePlan{ExternalCopiesNotPurged: true},
		candidateMaterials: make(map[string]string),
		runCandidates:      make(map[string]string),
	}
	plan := &selection.plan
	requested := make(map[string]bool)
	for _, id := range ids {
		requested[id] = true
	}
	plan.CandidateIDs = slices.Sorted(maps.Keys(requested))
	var err error
	plan.Revision, err = counter(tx, "data_revision")
	if err != nil {
		return selection, err
	}
	config, err := readOperatingConfig(tx)
	if err != nil {
		return selection, err
	}
	cutoff := now.Add(-config[evidenceRetention])
	candidates, err := listJSON[Candidate](tx, candidateBucket)
	if err != nil {
		return selection, err
	}
	eligible := make(map[string]bool)
	for _, candidate := range candidates {
		if !candidate.CreatedAt.After(cutoff) {
			eligible[candidate.ID] = true
		}
	}
	removeBaseline, removeManifest := make(map[string]bool), make(map[string]bool)
	keepBaseline, keepManifest := make(map[string]bool), make(map[string]bool)
	for _, candidate := range candidates {
		if requested[candidate.ID] && eligible[candidate.ID] {
			selection.candidateMaterials[candidate.ID] = candidate.MaterialDigest
			removeBaseline[candidate.BaselineDigest] = true
			removeManifest[candidate.ManifestDigest] = true
			continue
		}
		keepBaseline[candidate.BaselineDigest] = true
		keepManifest[candidate.ManifestDigest] = true
	}
	for _, id := range plan.CandidateIDs {
		if !eligible[id] {
			plan.Ineligible = append(plan.Ineligible, id)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(removeBaseline)) {
		if !keepBaseline[key] {
			plan.OrphanBaselines = append(plan.OrphanBaselines, key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(removeManifest)) {
		if !keepManifest[key] {
			plan.OrphanManifests = append(plan.OrphanManifests, key)
		}
	}
	receipts, err := listJSON[Receipt](tx, receiptBucket)
	if err != nil {
		return selection, err
	}
	blockers := make(map[string]bool)
	for _, receipt := range receipts {
		references := receipt.CandidateIDs
		if !slices.ContainsFunc(references, func(id string) bool { return requested[id] }) {
			continue
		}
		plan.ReceiptIDs = append(plan.ReceiptIDs, receipt.ID)
		for _, related := range references {
			if !requested[related] || !eligible[related] {
				blockers[related] = true
			}
		}
	}
	plan.SharedBlockers = slices.Sorted(maps.Keys(blockers))
	reports, err := listJSON[Report](tx, reportBucket)
	if err != nil {
		return selection, err
	}
	for _, report := range reports {
		if requested[report.CandidateID] && eligible[report.CandidateID] {
			plan.ReportIDs = append(plan.ReportIDs, report.ID)
		}
	}
	history, err := readHistoricalAI(tx, now)
	if err != nil {
		return selection, err
	}
	if history != nil {
		for _, request := range history.Requests {
			if requested[request.CandidateID] && eligible[request.CandidateID] {
				plan.AIRequestIDs = append(plan.AIRequestIDs, request.ID)
			}
		}
	}
	for _, id := range plan.CandidateIDs {
		if !eligible[id] {
			continue
		}
		runs, err := candidateRuns(tx, id)
		if err != nil {
			return selection, err
		}
		for _, run := range runs {
			plan.Runs = append(plan.Runs, run.ID)
			selection.runCandidates[run.ID] = run.CandidateID
		}
	}
	audit, err := listJSON[AuditEvent](tx, auditBucket)
	if err != nil {
		return selection, err
	}
	auditCutoff := now.Add(-config[auditRetention])
	for _, event := range audit {
		if !event.Time.After(auditCutoff) {
			plan.AuditSequences = append(plan.AuditSequences, event.Sequence)
		}
	}
	_, confirmation, err := recordset.EncodeJSON(selection.plan)
	plan.Confirmation = string(confirmation)
	return selection, err
}

func (b *Board) Purge(actor, project string, candidateIDs []string, confirmation string) (PurgePlan, error) {
	var plan PurgePlan
	err := b.change(actor, "retention.purge", func(tx *bolt.Tx) error {
		if err := b.authorizeCustody(tx, actor, project); err != nil {
			return err
		}
		now := b.now()
		selection, err := planPurge(tx, candidateIDs, now)
		plan = selection.plan
		if err != nil {
			return err
		}
		if plan.Confirmation != confirmation {
			return fmt.Errorf("%w: obtain a new purge preview", ErrConflict)
		}
		if len(plan.Ineligible) > 0 || len(plan.SharedBlockers) > 0 {
			return invalid("purge scope contains ineligible candidates or shared evidence")
		}
		if err := applyPurge(tx, selection, now); err != nil {
			return err
		}
		if _, err := increment(tx, "data_revision"); err != nil {
			return err
		}
		references := append(slices.Clone(plan.CandidateIDs), "receipts="+strconv.Itoa(len(plan.ReceiptIDs)),
			"runs="+strconv.Itoa(len(plan.Runs)), "reports="+strconv.Itoa(len(plan.ReportIDs)), "audit="+strconv.Itoa(len(plan.AuditSequences)))
		if len(plan.AIRequestIDs) != 0 {
			references = append(references, "ai_requests="+strconv.Itoa(len(plan.AIRequestIDs)))
		}
		return b.audit(tx, actor, "retention.purge", "success", references...)
	})
	return plan, b.recordIntegrityIssue(actor, err)
}

func applyPurge(tx *bolt.Tx, selection purgeSelection, at time.Time) error {
	plan := selection.plan
	for _, id := range plan.AIRequestIDs {
		if err := recordset.Delete(tx, recordset.Key{Bucket: aiHistoryBucket, Key: id},
			recordset.Key{Bucket: aiHistoryBinding, Key: id}); err != nil {
			return err
		}
	}
	for _, id := range plan.Runs {
		if err := tx.Bucket([]byte(purgedRunBucket)).Put([]byte(id), []byte("purged")); err != nil {
			return err
		}
		if err := recordset.Delete(tx, recordset.Key{Bucket: candidateRun, Key: selection.runCandidates[id] + "\x00" + id},
			recordset.Key{Bucket: runBucket, Key: id}); err != nil {
			return err
		}
	}
	for _, id := range plan.CandidateIDs {
		if err := tx.Bucket([]byte(tombstoneBucket)).Put([]byte(id), []byte(FormatUTC(at))); err != nil {
			return err
		}
		if err := recordset.Delete(tx, recordset.Key{Bucket: materialBucket, Key: selection.candidateMaterials[id]},
			recordset.Key{Bucket: candidateBucket, Key: id}, recordset.Key{Bucket: candidateMetaBucket, Key: id}); err != nil {
			return err
		}
	}
	var hashes []string
	for _, id := range plan.ReceiptIDs {
		hashes = append(hashes, receiptHash(id))
	}
	std := map[string][]string{
		receiptBucket: hashes, csvBucket: hashes, reportBucket: plan.ReportIDs, htmlBucket: plan.ReportIDs,
		reportMetaBucket: plan.ReportIDs,
		baselineBucket:   plan.OrphanBaselines, baselineMetaBucket: plan.OrphanBaselines, manifestBucket: plan.OrphanManifests,
	}
	for bucket, keys := range std {
		for _, key := range keys {
			if err := tx.Bucket([]byte(bucket)).Delete([]byte(key)); err != nil {
				return err
			}
		}
	}
	for _, sequence := range plan.AuditSequences {
		key := auditKey(sequence)
		if err := recordset.Delete(tx, recordset.Key{Bucket: auditBucket, Key: key},
			recordset.Key{Bucket: auditMetaBucket, Key: key}); err != nil {
			return err
		}
	}
	previews := tx.Bucket([]byte(previewBucket))
	var invalidTokens []string
	if err := previews.ForEach(func(key, _ []byte) error {
		binding, err := getJSON[previewBinding](tx, previewBucket, string(key))
		if err != nil {
			return err
		}
		if slices.Contains(hashes, binding.Hash) || slices.Contains(plan.CandidateIDs, binding.CandidateID) {
			invalidTokens = append(invalidTokens, string(key))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, token := range invalidTokens {
		if err := recordset.Delete(tx, recordset.Key{Bucket: previewBucket, Key: token},
			recordset.Key{Bucket: previewCSV, Key: token}); err != nil {
			return err
		}
	}
	return nil
}
