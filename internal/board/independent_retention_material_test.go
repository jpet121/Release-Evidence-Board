package board_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	bolt "go.etcd.io/bbolt"
	"release-evidence-board/internal/board"
)

type retainedFixtureKey struct{ bucket, key string }

func (e *independentEnvironment) retainedFingerprint(path string, keys []retainedFixtureKey) map[retainedFixtureKey]string {
	e.t.Helper()
	found := map[retainedFixtureKey]string{}
	inspectDetachedStore(e.t, path, func(tx *bolt.Tx, _ []byte) {
		for _, wanted := range keys {
			if raw := tx.Bucket([]byte(wanted.bucket)).Get([]byte(wanted.key)); raw != nil {
				found[wanted] = independentSHA(raw)
			}
		}
	})
	return found
}

func (e *independentEnvironment) checkPurgeOwnershipFault() {
	t := e.t
	expect := independentDecode[struct {
		Ownership, Timing, Clock string
		MixedCSV                 bool
	}](t, e.scenario.Expected)
	e.now = independentTime(t, expect.Clock)
	retained, _, err := e.b.CreateCandidate(independentAuthor, independentProject, board.CandidateInput{
		Manifest: independentManifest(t, e.scenario.Setup.SecondaryManifest.raw(t)), BaselineDigest: e.baseline.Descriptor.Digest})
	independentRequire(t, err)
	scope := []string{e.candidate.ID}
	var oldConfirmation string
	if expect.Timing == "confirmed before shared import" {
		cleanScope, err := e.b.PreviewPurge(independentOwner, independentProject, scope)
		independentRequire(t, err)
		if cleanScope.Confirmation == "" || len(cleanScope.Ineligible) != 0 || len(cleanScope.SharedBlockers) != 0 {
			t.Fatal("A-only scope should be independently confirmable before the shared original exists")
		}
		oldConfirmation = cleanScope.Confirmation
	}
	var receipt board.Receipt
	upload := "mixed"
	if expect.MixedCSV {
		receipt = e.commit(upload)
	} else {
		upload = "bOnly"
		e.commit("aOnly")
		e.preview(upload, retained.ID)
		receipt = e.confirm(upload)
	}
	proof, err := e.b.Freeze(independentOwner, independentProject, retained.ID)
	independentRequire(t, err)
	independentEqual(t, "fresh B has its own sufficient report", proof.Status, "SUFFICIENT")
	healthy, err := e.b.PreviewPurge(independentOwner, independentProject, scope)
	independentRequire(t, err)
	independentEqual(t, "A is exactly eligible", len(healthy.Ineligible), 0)
	if expect.MixedCSV {
		independentEqual(t, "intact shared CSV scopes its unexpired blocker", healthy.SharedBlockers, []string{retained.ID})
	} else if healthy.Confirmation == "" || len(healthy.SharedBlockers) != 0 || slices.Contains(healthy.ReportIDs, proof.ID) {
		t.Fatal("healthy A-only plan was not independently confirmable or included B's report")
	}
	if oldConfirmation == "" {
		oldConfirmation = healthy.Confirmation
	}
	mutation := func(tx *bolt.Tx) error {
		if expect.Ownership == "receipt" || expect.Ownership == "both" {
			// Preserve the original immutable CSV and every other field.
			if err := changeFixtureStringField(tx, "receipts", receipt.Hash, "CandidateIDs", []string{e.candidate.ID}); err != nil {
				return err
			}
		}
		if expect.Ownership == "report" || expect.Ownership == "both" {
			return changeFixtureStringField(tx, "reports", proof.ID, "CandidateID", e.candidate.ID)
		}
		return nil
	}
	app := e.b
	path := e.path
	if expect.Timing == "before preview" {
		_, path = e.faultedBackup(mutation)
		app, err = board.Open(path, e.settings())
		independentRequire(t, err)
		defer app.Close()
		health, err := app.Health(independentOwner, independentProject)
		independentRequire(t, err)
		independentEqual(t, "detached corruption already held after Open", health.Hold, true)
	} else {
		independentRequire(t, board.MutateIndependentFixtureStore(e.b, mutation))
	}
	keys := []retainedFixtureKey{
		{"candidates", e.candidate.ID}, {"candidates", retained.ID}, {"runs", "purge-a"}, {"runs", "purge-b"}, {"results", "B-R"},
		{"receipts", receipt.Hash}, {"csv_bytes", receipt.Hash}, {"reports", proof.ID}, {"report_html", proof.ID}, {"report_metadata_sha256", proof.ID},
	}
	before := e.retainedFingerprint(path, keys)
	independentEqual(t, "all designated protected records physically present before requested purge", len(before), len(keys))
	independentEqual(t, "protected original CSV matches literal input", before[retainedFixtureKey{"csv_bytes", receipt.Hash}], e.scenario.Setup.Uploads[upload].SHA256)
	independentEqual(t, "frozen HTML matches original checksum", before[retainedFixtureKey{"report_html", proof.ID}], proof.SHA256)
	if expect.Timing == "before preview" {
		plan, previewErr := app.PreviewPurge(independentOwner, independentProject, scope)
		independentEqual(t, "untrusted custody blocks fresh preview", errors.Is(previewErr, board.ErrIntegrity), true)
		independentEqual(t, "no usable confirmation published", plan.Confirmation == "", true)
		if plan.Confirmation != "" && len(plan.SharedBlockers) == 0 && len(plan.Ineligible) == 0 {
			// The pre-repair app can offer a destructive confirmation. Use
			// only the synthetic detached copy to record its physical effect.
			_, confirmErr := app.Purge(independentOwner, independentProject, scope, plan.Confirmation)
			independentEqual(t, "untrusted custody blocks fresh confirm", errors.Is(confirmErr, board.ErrIntegrity), true)
		}
	} else {
		_, purgeErr := app.Purge(independentOwner, independentProject, scope, oldConfirmation)
		independentEqual(t, "pre-fault confirmation cannot authorize deletion after custody changes", errors.Is(purgeErr, board.ErrIntegrity), true)
	}
	after := e.retainedFingerprint(path, keys)
	for _, key := range keys {
		independentEqual(t, "physical preservation of "+key.bucket+"/"+key.key, after[key], before[key])
	}
	health, err := app.Health(independentOwner, independentProject)
	independentRequire(t, err)
	independentEqual(t, "unsafe custody keeps integrity hold", health.Hold, true)
	if after[retainedFixtureKey{"report_html", proof.ID}] != "" {
		_, protectedHTML, readErr := app.ReportHTML(independentOwner, independentProject, proof.ID)
		independentRequire(t, readErr)
		independentEqual(t, "original historical bytes remain readable while assessment is held", independentSHA(protectedHTML), proof.SHA256)
	}
	t.Logf("compared %d protected primary values, including receipt, raw CSV, report and detached report binding", len(keys))
}

func changeFixtureStringField(tx *bolt.Tx, bucketName, key, field string, value any) error {
	bucket := tx.Bucket([]byte(bucketName))
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(bucket.Get([]byte(key)), &stored); err != nil {
		return err
	}
	if stored[field] == nil {
		return fmt.Errorf("fixture does not have expected field %s", field)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	stored[field] = encoded
	return replaceDetachedJSON(bucket, key, stored)
}

func (e *independentEnvironment) checkMaterialCustody() {
	t := e.t
	variant := independentDecode[struct{ Variant string }](t, e.scenario.Expected).Variant
	first := e.commit("failed")
	beforeA := e.assess(e.candidate.ID, "")
	independentEqual(t, "A starts with definitive failure", beforeA.Status, "BLOCKED")
	corrected, reused, err := e.b.CreateCandidate(independentAuthor, independentProject, board.CandidateInput{
		Manifest: independentManifest(t, e.scenario.Setup.SecondaryManifest.raw(t)), BaselineDigest: e.baseline.Descriptor.Digest,
		PredecessorID: e.candidate.ID, Correction: "Controlled environment revision E1 to E2"})
	independentRequire(t, err)
	independentEqual(t, "different environment creates new assessment", reused, false)
	independentEqual(t, "real correction links predecessor", corrected.PredecessorID, e.candidate.ID)
	if len(corrected.Changes) == 0 {
		t.Error("real correction should retain changed execution inputs")
	}
	independentEqual(t, "real correction starts with missing evidence", e.assess(corrected.ID, "").Status, "BLOCKED")
	e.preview("corrected", corrected.ID)
	other := e.confirm("corrected")
	independentEqual(t, "only fresh evidence satisfies genuine correction", e.assess(corrected.ID, "").Status, "SUFFICIENT")
	independentEqual(t, "real correction cannot clear prior failure", e.assess(e.candidate.ID, "").Status, "BLOCKED")
	label := board.CandidateInput{Manifest: independentManifest(t, e.scenario.Setup.DescriptorInput.raw(t)), BaselineDigest: e.baseline.Descriptor.Digest,
		PredecessorID: e.candidate.ID, Correction: "Label update only"}
	switch variant {
	case "healthy corrected execution":
		reply, reuse, err := e.b.CreateCandidate(independentAuthor, independentProject, label)
		independentRequire(t, err)
		independentEqual(t, "healthy label-only update reuses retained A", reuse && reply.ID == e.candidate.ID, true)
		return
	case "legitimate tombstone":
		e.now = independentTime(t, "2027-05-16T12:00:00Z")
		// Both pairs are old in this fixture, but only A is explicitly scoped.
		scope := []string{e.candidate.ID}
		plan, err := e.b.PreviewPurge(independentOwner, independentProject, scope)
		independentRequire(t, err)
		_, err = e.b.Purge(independentOwner, independentProject, scope, plan.Confirmation)
		independentRequire(t, err)
		_, _, err = e.b.CreateCandidate(independentAuthor, independentProject, board.CandidateInput{Manifest: e.manifest, BaselineDigest: e.baseline.Descriptor.Digest})
		independentEqual(t, "explicitly purged exact pair remains tombstoned", errors.Is(err, board.ErrConflict), true)
		health, err := e.b.Health(independentOwner, independentProject)
		independentRequire(t, err)
		independentEqual(t, "intentional purge does not masquerade as material corruption", health.Hold, false)
		original, err := e.b.SourceCSV(independentOwner, independentProject, other.ID)
		independentRequire(t, err)
		independentEqual(t, "valid retention preserves out-of-scope bytes", original, e.raw("corrected"))
		return
	}
	independentRequire(t, board.MutateIndependentFixtureStore(e.b, func(tx *bolt.Tx) error {
		material := tx.Bucket([]byte("material"))
		if material.Get([]byte(e.candidate.MaterialDigest)) == nil {
			return errors.New("fixture lacks original material index key")
		}
		switch variant {
		case "missing material index":
			return material.Delete([]byte(e.candidate.MaterialDigest))
		case "material index redirected to unrelated passing candidate":
			return replaceDetachedJSON(material, e.candidate.MaterialDigest, corrected.ID)
		default:
			return fmt.Errorf("unknown custody variant %q", variant)
		}
	}))
	keys := []retainedFixtureKey{
		{"candidates", e.candidate.ID}, {"candidates", corrected.ID}, {"runs", "failed"}, {"runs", "corrected-pass"},
		{"receipts", first.Hash}, {"csv_bytes", first.Hash}, {"receipts", other.Hash}, {"csv_bytes", other.Hash},
	}
	before := e.retainedFingerprint(e.path, keys)
	independentEqual(t, "fault affects only the derived material index", len(before), len(keys))
	reply, reuse, err := e.b.CreateCandidate(independentAuthor, independentProject, label)
	independentEqual(t, "damaged material index rejects label-only creation or reuse", errors.Is(err, board.ErrIntegrity), true)
	if err == nil && !reuse {
		// Show how a pre-repair label-only duplicate can obtain a false
		// sufficient status, with a synthetic source fixture and isolated DB.
		e.preview("labelOnly", reply.ID)
		e.confirm("labelOnly")
		unexpected := e.assess(reply.ID, "")
		t.Errorf("label-only duplicate gained status %s with %d recorded material changes", unexpected.Status, len(reply.Changes))
		published, freezeErr := e.b.Freeze(independentOwner, independentProject, reply.ID)
		if freezeErr == nil {
			t.Errorf("label-only duplicate exported a frozen %s report", published.Status)
		}
	}
	independentEqual(t, "wrong reuse result is never returned", reply.ID == "", true)
	health, err := e.b.Health(independentOwner, independentProject)
	independentRequire(t, err)
	independentEqual(t, "derived index corruption holds assessments", health.Hold, true)
	if health.Hold {
		_, err = e.b.Assess(independentOwner, independentProject, corrected.ID)
		independentEqual(t, "global hold also blocks a fresh decision for good B", errors.Is(err, board.ErrIntegrity), true)
	}
	independentEqual(t, "known candidate and original evidence records unchanged", e.retainedFingerprint(e.path, keys), before)
	inspectDetachedStore(t, e.path, func(tx *bolt.Tx, _ []byte) {
		independentEqual(t, "no new retained candidate", tx.Bucket([]byte("candidates")).Stats().KeyN, 2)
		independentEqual(t, "no new receipt", tx.Bucket([]byte("receipts")).Stats().KeyN, 2)
	})
}
