package board

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	bolt "go.etcd.io/bbolt"
)

func verifiedCandidateRecords(tx *bolt.Tx, id string) (Candidate, StoredManifest, ApprovedBaseline, error) {
	candidate, manifest, baseline, err := canonicalCandidateRecords(tx, id)
	if err != nil {
		return Candidate{}, StoredManifest{}, ApprovedBaseline{}, err
	}
	material, err := checkedMaterialDigest(candidate, manifest, baseline)
	if err != nil {
		return Candidate{}, StoredManifest{}, ApprovedBaseline{}, err
	}
	mapped, err := getJSON[string](tx, materialBucket, material)
	if err != nil || mapped != id {
		return Candidate{}, StoredManifest{}, ApprovedBaseline{}, integrity("candidate material index does not match canonical records")
	}
	return candidate, manifest, baseline, nil
}

func canonicalCandidateRecords(tx *bolt.Tx, id string) (Candidate, StoredManifest, ApprovedBaseline, error) {
	candidate, manifest, baseline, err := candidateRecords(tx, id)
	if err != nil {
		return Candidate{}, StoredManifest{}, ApprovedBaseline{}, err
	}
	manifestDescriptor, manifestError := manifest.Manifest.Descriptor()
	product := string(tx.Bucket([]byte(metaBucket)).Get([]byte("product")))
	if manifestError != nil || manifestDescriptor != manifest.Descriptor || manifestDescriptor.Digest != candidate.ManifestDigest ||
		id != candidate.ID || id != "C-"+digest([]byte(manifestDescriptor.Digest+baseline.Descriptor.Digest)) ||
		manifest.Manifest.ProductID != product {
		return Candidate{}, StoredManifest{}, ApprovedBaseline{}, integrity("candidate descriptors or identity differ from approved bytes")
	}
	return candidate, manifest, baseline, nil
}

func checkedMaterialDigest(candidate Candidate, manifest StoredManifest, baseline ApprovedBaseline) (string, error) {
	material, err := materialDescriptor(manifest.Manifest, baseline.Baseline)
	if err != nil || material.Digest != candidate.MaterialDigest {
		return "", integrity("candidate material identity is inconsistent")
	}
	return material.Digest, nil
}

func verifyApprovedBaseline(tx *bolt.Tx, baseline ApprovedBaseline, key, product string) error {
	descriptor, err := baseline.Baseline.Descriptor()
	if err != nil || descriptor != baseline.Descriptor || descriptor.Digest != key || baseline.Baseline.ProductID != product {
		return integrity("approved baseline differs from canonical bytes")
	}
	if !idPattern.MatchString(baseline.Approver) {
		return integrity("approved baseline lacks a valid recorded reviewer")
	}
	for _, change := range baseline.Thresholds {
		if len(change.ReducingActors) == 0 {
			return integrity("approved threshold reduction lacks its recorded actor")
		}
		seen := make(map[string]bool, len(change.ReducingActors))
		for _, actor := range change.ReducingActors {
			if !idPattern.MatchString(actor) || seen[actor] {
				return integrity("approved threshold reduction contains an invalid or repeated actor")
			}
			if actor == baseline.Approver {
				return integrity("approved threshold reduction names its approver as a reducing actor")
			}
			seen[actor] = true
		}
	}
	return checkRecordBinding(tx, baselineBucket, baselineMetaBucket, key)
}

func getApprovedBaseline(tx *bolt.Tx, key string) (ApprovedBaseline, error) {
	baseline, err := getJSON[ApprovedBaseline](tx, baselineBucket, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) && tx.Bucket([]byte(baselineMetaBucket)).Get([]byte(key)) != nil {
			err = integrity("retained approved baseline record is missing")
		}
		return ApprovedBaseline{}, err
	}
	product := string(tx.Bucket([]byte(metaBucket)).Get([]byte("product")))
	if err := verifyApprovedBaseline(tx, baseline, key, product); err != nil {
		return ApprovedBaseline{}, err
	}
	return baseline, nil
}

func visitApprovedBaselines(tx *bolt.Tx, product string, visit func(ApprovedBaseline) error) error {
	if err := verifyCompanionKeys(tx, baselineMetaBucket, baselineBucket); err != nil {
		return err
	}
	return visitJSON(tx, baselineBucket, func(key string, baseline ApprovedBaseline) error {
		if err := verifyApprovedBaseline(tx, baseline, key, product); err != nil {
			return err
		}
		return visit(baseline)
	})
}

func validateDescriptorIndexes(tx *bolt.Tx) error {
	return visitDescriptorCatalog(tx, nil)
}

// The consumer receives decoded values after each row's custody checks. The
// caller must not use the selection until the whole catalog has been verified.
func visitDescriptorCatalog(tx *bolt.Tx, visit func(Candidate, StoredManifest, ApprovedBaseline)) error {
	if err := verifyCompanionKeys(tx, candidateMetaBucket, candidateBucket); err != nil {
		return err
	}
	product := string(tx.Bucket([]byte(metaBucket)).Get([]byte("product")))
	materialIDs := make(map[string]string)
	manifestIDs := make(map[string]bool)
	err := visitJSON(tx, candidateBucket, func(key string, record Candidate) error {
		_, manifest, baseline, err := canonicalCandidateRecords(tx, key)
		if err != nil {
			return err
		}
		material, err := checkedMaterialDigest(record, manifest, baseline)
		if err != nil || materialIDs[material] != "" {
			return integrity("candidate material identity is inconsistent")
		}
		if tx.Bucket([]byte(tombstoneBucket)).Get([]byte(key)) != nil {
			return integrity("retained candidate also has a purge marker")
		}
		if record.PredecessorID != "" && (record.PredecessorID == key || !retainedCandidateReference(tx, record.PredecessorID)) {
			return integrity("candidate predecessor reference is inconsistent")
		}
		materialIDs[material] = key
		manifestIDs[manifest.Descriptor.Digest] = true
		if visit != nil {
			visit(record, manifest, baseline)
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = visitJSON(tx, materialBucket, func(key, id string) error {
		if id == "" || materialIDs[key] != id {
			return integrity("candidate material index is inconsistent")
		}
		delete(materialIDs, key)
		return nil
	})
	if err != nil {
		return err
	}
	if len(materialIDs) != 0 {
		return integrity("candidate material index omits retained candidates")
	}
	err = visitJSON(tx, manifestBucket, func(key string, manifest StoredManifest) error {
		if manifest.Descriptor.Digest != key || !manifestIDs[key] {
			return integrity("manifest has no matching candidate and digest key")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return validateBaselineAndDraftIndexes(tx, product)
}

func validateBaselineAndDraftIndexes(tx *bolt.Tx, product string) error {
	// The revision ledger and approved draft survive an eligible purge of
	// an unshared approved descriptor. Check the remaining links.
	ledger := make(map[string]string)
	immutable := map[string]map[string][]byte{behaviorRevision: {}, policyRevision: {}, testOwners: {}}
	lastDraft := tx.Bucket([]byte(draftBucket)).Sequence()
	err := visitRetainedDrafts(tx, product, func(draft Draft) error {
		sequence, err := strconv.ParseUint(strings.TrimPrefix(draft.ID, "D-"), 10, 64)
		if err != nil || sequence == 0 || draft.ID != "D-"+strconv.FormatUint(sequence, 10) || sequence > lastDraft {
			return integrity("draft allocator and retained draft identifiers are inconsistent")
		}
		if draft.ApprovedDigest == "" {
			return nil
		}
		descriptor, err := draft.Baseline.Descriptor()
		revision := versionKey(draft.Baseline.ID, draft.Baseline.Revision)
		if err != nil || descriptor.Digest != draft.ApprovedDigest || ledger[revision] != "" {
			return integrity("approved draft differs from its original descriptor or revision")
		}
		ledger[revision] = descriptor.Digest
		return visitBaselineIdentities(draft.Baseline, func(bucket, key string, value any) error {
			return rememberImmutableIdentity(immutable[bucket], key, value)
		})
	})
	if err != nil {
		return err
	}
	revisions := make(map[string]string)
	err = visitJSON(tx, baselineRevision, func(key, digest string) error {
		if digest == "" || ledger[key] != digest {
			return integrity("baseline revision index is inconsistent")
		}
		revisions[key] = digest
		return nil
	})
	if err != nil {
		return err
	}
	if len(revisions) != len(ledger) {
		return integrity("baseline revision index omits approved drafts")
	}
	for bucket, expected := range immutable {
		if err := tx.Bucket([]byte(bucket)).ForEach(func(key, fingerprint []byte) error {
			if canonical, found := expected[string(key)]; !found || !bytes.Equal(canonical, fingerprint) {
				return integrity("immutable identity index differs from approved drafts")
			}
			delete(expected, string(key))
			return nil
		}); err != nil {
			return err
		}
		if len(expected) != 0 {
			return integrity("immutable identity index omits approved draft records")
		}
	}
	return visitApprovedBaselines(tx, product, func(approved ApprovedBaseline) error {
		return verifyApprovedDraftLink(tx, approved, revisions)
	})
}

// rememberImmutableIdentity builds the independent expected ledger. It rejects
// unencodable or conflicting values before changing the expected identity.
func rememberImmutableIdentity(expected map[string][]byte, key string, value any) error {
	fingerprint, err := immutableFingerprint(value)
	if err != nil {
		return err
	}
	prior := expected[key]
	if prior != nil && !bytes.Equal(prior, fingerprint) {
		return integrity("approved drafts disagree on an immutable identity")
	}
	expected[key] = fingerprint
	return nil
}

func verifyApprovedDraftLink(tx *bolt.Tx, approved ApprovedBaseline, revisions map[string]string) error {
	key := approved.Descriptor.Digest
	draft, err := getRetainedDraft(tx, approved.DraftID)
	revision := versionKey(approved.Baseline.ID, approved.Baseline.Revision)
	if err != nil || draft.ApprovedDigest != key || draft.Author != approved.Author || revisions[revision] != key {
		return integrity("approved baseline lacks the matching source draft and revision")
	}
	return nil
}

// validateRunCustody keeps metadata/result validation before the final native
// candidate reference check, independent of a caller's catalog preflight.
func validateRunCustody(tx *bolt.Tx, run, expected Run) error {
	if err := validateRetainedRun(tx, run, expected); err != nil {
		return err
	}
	if tx.Bucket([]byte(candidateBucket)).Get([]byte(run.CandidateID)) == nil {
		return integrity("retained run references a missing candidate")
	}
	return nil
}

func verifyDraftRecord(tx *bolt.Tx, key string, draft Draft, product string) error {
	if draft.ID != key || !idPattern.MatchString(key) || draft.Revision == 0 || draft.Baseline.ProductID != product {
		return integrity("draft key, revision or product is inconsistent")
	}
	return checkRecordBinding(tx, draftBucket, draftMetaBucket, key)
}

func visitRetainedDrafts(tx *bolt.Tx, product string, visit func(Draft) error) error {
	if err := verifyCompanionKeys(tx, draftMetaBucket, draftBucket); err != nil {
		return err
	}
	return visitJSON(tx, draftBucket, func(key string, draft Draft) error {
		if err := verifyDraftRecord(tx, key, draft, product); err != nil {
			return err
		}
		return visit(draft)
	})
}

func getRetainedDraft(tx *bolt.Tx, key string) (Draft, error) {
	draft, err := getJSON[Draft](tx, draftBucket, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) && tx.Bucket([]byte(draftMetaBucket)).Get([]byte(key)) != nil {
			err = integrity("retained draft record is missing")
		}
		return Draft{}, err
	}
	product := string(tx.Bucket([]byte(metaBucket)).Get([]byte("product")))
	if err := verifyDraftRecord(tx, key, draft, product); err != nil {
		return Draft{}, err
	}
	return draft, nil
}

func verifyCompanionKeys(tx *bolt.Tx, companions, owners string) error {
	index := tx.Bucket([]byte(owners))
	return tx.Bucket([]byte(companions)).ForEach(func(key, _ []byte) error {
		if index.Get(key) == nil {
			return integrity("stored evidence companion lacks a primary record")
		}
		return nil
	})
}

func retainedCandidateReference(tx *bolt.Tx, id string) bool {
	key := []byte(id)
	return tx.Bucket([]byte(candidateBucket)).Get(key) != nil || tx.Bucket([]byte(tombstoneBucket)).Get(key) != nil
}

func integrity(reason string) error { return fmt.Errorf("%w: %s", ErrIntegrity, reason) }

func verifiedCandidateRuns(tx *bolt.Tx, id string) ([]Run, uint64, error) {
	runs, err := candidateRuns(tx, id)
	if err != nil {
		return nil, 0, err
	}
	expected, ledger, err := expectedStoredRuns(tx, id)
	if err != nil {
		return nil, 0, err
	}
	if len(runs) != len(expected) {
		return nil, 0, integrity("candidate run index omits original CSV evidence")
	}
	for _, run := range runs {
		if err := validateRetainedRun(tx, run, expected[run.ID]); err != nil {
			return nil, 0, err
		}
		delete(expected, run.ID)
	}
	if _, err := uniqueRunResults(runs); err != nil {
		return nil, 0, err
	}
	return runs, ledger, nil
}

// Retained receipt and report metadata witness a minimum product-wide ledger.
// Import planning checks this bound without reopening their CSV or HTML.
func checkLedgerWitnesses(tx *bolt.Tx, sequence uint64) error {
	type witness struct {
		LedgerHighWater uint64
	}
	for _, bucket := range []string{receiptBucket, reportBucket} {
		if tx.Bucket([]byte(bucket)) == nil {
			return integrity("retained ledger witness storage section is missing")
		}
		if err := visitJSON(tx, bucket, func(_ string, item witness) error {
			return checkLedgerWitness(sequence, item.LedgerHighWater)
		}); err != nil {
			return err
		}
	}
	return nil
}

func checkLedgerWitness(sequence, retained uint64) error {
	if retained > sequence {
		return integrity("ledger allocator precedes retained evidence")
	}
	return nil
}

// Each receipt binds original bytes. Its AddedRuns positions bind product-wide
// sequence numbers. Reconstruct from this source independently of the run index.
func expectedStoredRuns(tx *bolt.Tx, onlyCandidate string) (map[string]Run, uint64, error) {
	runs := make(map[string]Run)
	ledger, err := counter(tx, "ledger")
	if err != nil {
		return nil, 0, err
	}
	err = visitJSON(tx, receiptBucket, func(key string, receipt Receipt) error {
		if err := checkReceiptIdentity(key, receipt); err != nil {
			return err
		}
		if onlyCandidate != "" && !slices.Contains(receipt.CandidateIDs, onlyCandidate) {
			return nil
		}
		if receipt.LedgerHighWater > ledger || receipt.LedgerHighWater < uint64(len(receipt.AddedRuns)) {
			return integrity("receipt ledger is inconsistent")
		}
		groups, err := sourceRunGroups(tx, key, receipt)
		if err != nil {
			return err
		}
		for i, id := range receipt.AddedRuns {
			run := groups[id]
			if onlyCandidate != "" && run.CandidateID != onlyCandidate {
				continue
			}
			if _, repeated := runs[id]; repeated {
				return integrity("run has more than one originating receipt")
			}
			run.Sequence = receipt.LedgerHighWater - uint64(len(receipt.AddedRuns)) + uint64(i) + 1
			runs[id] = run
		}
		return nil
	})
	return runs, ledger, err
}

func retainedReceipt(tx *bolt.Tx, key string) (Receipt, []byte, error) {
	receipt, err := getJSON[Receipt](tx, receiptBucket, key)
	if err != nil {
		if errors.Is(err, ErrNotFound) && tx.Bucket([]byte(csvBucket)).Get([]byte(key)) != nil {
			return Receipt{}, nil, integrity("original CSV lacks its retained receipt")
		}
		return Receipt{}, nil, err
	}
	raw, err := receiptBytes(tx, key, receipt)
	if err != nil {
		return Receipt{}, nil, err
	}
	return receipt, raw, nil
}

func checkReceiptIdentity(key string, receipt Receipt) error {
	if !digestPattern.MatchString(key) || receipt.Hash != key || receipt.ID != "I-"+key {
		return integrity("retained receipt identity is inconsistent")
	}
	return nil
}

func receiptBytes(tx *bolt.Tx, key string, receipt Receipt) ([]byte, error) {
	if err := checkReceiptIdentity(key, receipt); err != nil {
		return nil, err
	}
	raw := tx.Bucket([]byte(csvBucket)).Get([]byte(key))
	if raw == nil || digest(raw) != key {
		return nil, integrity("original CSV is missing or its hash differs")
	}
	return raw, nil
}

func sourceRunGroups(tx *bolt.Tx, key string, receipt Receipt) (map[string]Run, error) {
	raw, err := receiptBytes(tx, key, receipt)
	if err != nil {
		return nil, err
	}
	rows, issues := ParseResultsCSV(raw)
	if len(issues) > 0 || len(rows) != receipt.Rows || receipt.Schema != ResultSchema {
		return nil, integrity("receipt row count or parser version differs from original CSV")
	}
	candidateIDs := map[string]bool{}
	groups := map[string]Run{}
	for _, row := range rows {
		fields := row.Fields
		candidateID := "C-" + digest([]byte(fields[colManifest]+fields[colBaseline]))
		candidateIDs[candidateID] = true
		closed, _ := parseTimestamp(row.ClosedAt())
		run, found := groups[row.RunID()]
		if found {
			if compareRunRow(row, run, candidateID, closed) != nil {
				return nil, integrity("original CSV contains conflicting run metadata")
			}
		} else {
			run = Run{ID: row.RunID(), CandidateID: candidateID, BehaviorID: fields[colBehavior], Closed: closed,
				ImportedAt: receipt.ImportedAt, ReceiptID: receipt.ID, FileHash: receipt.Hash}
		}
		run.Rows = append(run.Rows, row)
		groups[run.ID] = run
	}
	if !slices.Equal(slices.Sorted(maps.Keys(candidateIDs)), receipt.CandidateIDs) {
		return nil, integrity("receipt candidate identities differ from original CSV")
	}
	if !retainedCandidateReference(tx, receipt.OpenCandidateID) ||
		!slices.Equal(historicalCandidates(receipt.CandidateIDs, receipt.OpenCandidateID), receipt.HistoricalCandidates) {
		return nil, integrity("receipt workspace or historical candidate references are inconsistent")
	}
	for id := range candidateIDs {
		if tx.Bucket([]byte(candidateBucket)).Get([]byte(id)) == nil {
			return nil, integrity("receipt lacks a retained evidence candidate")
		}
	}
	if !slices.IsSorted(receipt.AddedRuns) || !slices.IsSorted(receipt.ExistingRuns) {
		return nil, integrity("receipt run ledger ordering is inconsistent")
	}
	receiptIDs := append(slices.Clone(receipt.AddedRuns), receipt.ExistingRuns...)
	slices.Sort(receiptIDs)
	if !slices.Equal(slices.Sorted(maps.Keys(groups)), receiptIDs) {
		return nil, integrity("receipt run identities differ from original CSV")
	}
	previous := make(map[string]bool)
	for _, id := range receipt.ExistingRuns {
		stored, err := getJSON[Run](tx, runBucket, id)
		if err != nil || !identicalRows(stored.Rows, groups[id].Rows) {
			return nil, integrity("retained duplicate rows differ from original CSV")
		}
		previous[stored.ReceiptID] = true
	}
	if !slices.Equal(slices.Sorted(maps.Keys(previous)), receipt.PreviousReceipts) {
		return nil, integrity("receipt references differ from originating run receipts")
	}
	return groups, nil
}

func validateRetainedRun(tx *bolt.Tx, run, expected Run) error {
	if expected.ID == "" || run.ID != expected.ID || run.CandidateID != expected.CandidateID ||
		run.BehaviorID != expected.BehaviorID || !run.Closed.Equal(expected.Closed) || run.Sequence != expected.Sequence ||
		run.FileHash != expected.FileHash || run.ReceiptID != expected.ReceiptID || !run.ImportedAt.Equal(expected.ImportedAt) ||
		!slices.Equal(run.Rows, expected.Rows) {
		return integrity("run metadata or complete source row set differs from original CSV")
	}
	for _, row := range run.Rows {
		if row.Kind() == Result {
			id, err := getJSON[string](tx, resultBucket, row.ResultID())
			if err != nil || id != run.ID {
				return integrity("result index is inconsistent")
			}
		}
	}
	return nil
}

func uniqueRunResults(runs []Run) (map[string]string, error) {
	resultIDs := make(map[string]string)
	sequences := make(map[uint64]bool, len(runs))
	seenSlots := make(map[slot]bool)
	for _, run := range runs {
		if sequences[run.Sequence] {
			return nil, integrity("ledger sequence is duplicated")
		}
		sequences[run.Sequence] = true
		clear(seenSlots)
		for _, row := range run.Rows {
			if row.Kind() == Result {
				if _, repeated := resultIDs[row.ResultID()]; repeated {
					return nil, integrity("a result ID is duplicated in retained evidence")
				}
				attempt, _ := positiveDecimal(row.Attempt())
				key := slot{TestID: row.TestID(), Attempt: attempt}
				if seenSlots[key] {
					return nil, integrity("a test attempt has multiple results in a retained run")
				}
				seenSlots[key] = true
				resultIDs[row.ResultID()] = run.ID
			}
		}
	}
	return resultIDs, nil
}

func validateRunIndexes(tx *bolt.Tx, runs []Run) error {
	resultIDs, err := uniqueRunResults(runs)
	if err != nil {
		return err
	}
	byKey := make(map[string]string, len(runs))
	for _, run := range runs {
		byKey[run.CandidateID+"\x00"+run.ID] = run.ID
	}
	if err := tx.Bucket([]byte(candidateRun)).ForEach(func(key, value []byte) error {
		id, known := byKey[string(key)]
		if !known || id != string(value) {
			return integrity("candidate run index is inconsistent")
		}
		delete(byKey, string(key))
		return nil
	}); err != nil {
		return err
	}
	if len(byKey) != 0 {
		return integrity("candidate run index omits retained runs")
	}
	return tx.Bucket([]byte(resultBucket)).ForEach(func(key, value []byte) error {
		var runID string
		if err := json.Unmarshal(value, &runID); err != nil {
			return integrity("result index entry is invalid")
		}
		if resultIDs[string(key)] != runID && tx.Bucket([]byte(purgedRunBucket)).Get([]byte(runID)) == nil {
			return integrity("result index refers to a missing source row")
		}
		return nil
	})
}
