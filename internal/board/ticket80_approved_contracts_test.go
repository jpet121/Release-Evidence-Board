package board

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// These tests cross the transaction/contract seams explicitly approved for
// Ticket80. Invalid native records are temporary transaction inputs, never a
// claimed supported public state or a replacement for public custody tests.
func TestTicket80ApprovedPurgePlanningContractRejectsMalformedSections(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	ids := []string{f.candidate.ID}
	age := f.at.Add(181 * 24 * time.Hour)
	before, err := f.app.Assess(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, bucket, key string }{
		{"revision", metaBucket, "data_revision"},
		{"configuration", metaBucket, operatingConfigKey},
		{"candidate", candidateBucket, f.candidate.ID},
		{"receipt", receiptBucket, f.receipt.Hash},
		{"report", reportBucket, f.report.ID},
		{"historical-ai", metaBucket, aiSnapshotKey},
		{"run", runBucket, f.receipt.AddedRuns[0]},
		{"audit", auditBucket, auditKey(1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var plan PurgePlan
			err := f.app.db.Update(func(tx *bolt.Tx) error {
				if err := tx.Bucket([]byte(tc.bucket)).Put([]byte(tc.key), []byte("{")); err != nil {
					t.Fatal(err)
				}
				selection, err := planPurge(tx, ids, age)
				plan = selection.plan
				return err // Native failure must cause caller-owned rollback.
			})
			if !errors.Is(err, ErrIntegrity) || plan.Confirmation != "" || !reflect.DeepEqual(plan.CandidateIDs, ids) {
				t.Fatalf("purge contract accepted malformed %s or returned usable confirmation: %+v %v", tc.name, plan, err)
			}
			after, err := f.app.Assess(secondActor, secondProject, f.candidate.ID)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("failed planning changed retained assessment: %v", err)
			}
			if _, body, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || !bytes.Equal(body, f.html) {
				t.Fatalf("failed planning changed frozen report: %v", err)
			}
			if raw, err := f.app.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(raw, f.source) {
				t.Fatalf("failed planning lost shared source evidence: %v", err)
			}
		})
	}
}

func TestTicket80ApprovedPurgeApplicationContractRollsBackNativeRefusals(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	pending := f.preview(t)
	before, err := f.app.Assess(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, bucket, key string }{
		{"run-marker", purgedRunBucket, f.receipt.AddedRuns[0]},
		{"run-index", candidateRun, f.candidate.ID + "\x00" + f.receipt.AddedRuns[0]},
		{"candidate-marker", tombstoneBucket, f.candidate.ID},
		{"candidate-material", materialBucket, f.candidate.MaterialDigest},
		{"shared-receipt", receiptBucket, f.receipt.Hash},
		{"audit-binding", auditMetaBucket, auditKey(1)},
		{"preview-bytes", previewCSV, pending.Token},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := f.app.db.Update(func(tx *bolt.Tx) error {
				selection, err := planPurge(tx, []string{f.candidate.ID}, f.at.Add(181*24*time.Hour))
				if err != nil || len(selection.plan.Runs) != 1 || len(selection.plan.AuditSequences) == 0 {
					t.Fatalf("fixture lacks real eligible evidence selection: %+v %v", selection.plan, err)
				}
				bucket, key := tx.Bucket([]byte(tc.bucket)), []byte(tc.key)
				if err := bucket.Delete(key); err != nil {
					t.Fatal(err)
				}
				if _, err := bucket.CreateBucket(key); err != nil {
					t.Fatal(err)
				}
				return applyPurge(tx, selection, f.at.Add(181*24*time.Hour))
			})
			if !errors.Is(err, bolt.ErrIncompatibleValue) {
				t.Fatalf("purge contract suppressed real %s refusal instead of rolling back: %v", tc.name, err)
			}
			after, err := f.app.Assess(secondActor, secondProject, f.candidate.ID)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("native purge refusal changed retained assessment: %v", err)
			}
			if report, body, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(body, f.html) {
				t.Fatalf("native purge refusal changed frozen report: %v", err)
			}
			if raw, err := f.app.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(raw, f.source) {
				t.Fatalf("native purge refusal lost shared source evidence: %v", err)
			}
			if preview, err := f.app.db.Begin(false); err != nil {
				t.Fatal(err)
			} else {
				if !bytes.Equal(preview.Bucket([]byte(previewCSV)).Get([]byte(pending.Token)), f.pending) {
					t.Error("native purge refusal lost staged evidence")
				}
				if err := preview.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	f.checkReopened(t, false)
}

func TestTicket80ApprovedMaterialContractRejectsInvalidEncoding(t *testing.T) {
	valid := Baseline{Behaviors: []Behavior{{Text: "contract", Tests: []TestPlan{{ExpectedOutcome: "accepted", PlannedAttempts: 1}}}}}
	invalidTest := valid
	invalidTest.Behaviors = []Behavior{{Text: "contract", Tests: []TestPlan{{ExpectedOutcome: "\xff", PlannedAttempts: 1}}}}
	invalidBehavior := valid
	invalidBehavior.Behaviors = []Behavior{{Text: "\xff", Tests: []TestPlan{{ExpectedOutcome: "accepted", PlannedAttempts: 1}}}}
	for _, baseline := range []Baseline{invalidTest, invalidBehavior} {
		if descriptor, err := materialDescriptor(Manifest{}, baseline); err == nil || descriptor != (Descriptor{}) || !strings.Contains(err.Error(), "UTF-8") {
			t.Fatalf("material contract exposed a descriptor for invalid text: %+v %v", descriptor, err)
		}
	}
	for _, pair := range [][2]Baseline{{invalidTest, valid}, {valid, invalidBehavior}} {
		if changes, err := changedInputs(Manifest{}, Manifest{}, pair[0], pair[1]); err == nil || changes != nil || !strings.Contains(err.Error(), "UTF-8") {
			t.Fatalf("lineage contract hid an invalid before/after encoding: %v %v", changes, err)
		}
	}
	predecessor := candidatePredecessor{id: "retained", baseline: invalidBehavior}
	if id, changes, err := predecessor.resolve(CandidateInput{}, valid); err == nil || id != "" || changes != nil {
		t.Fatalf("predecessor contract exposed lineage after invalid material: %q %v %v", id, changes, err)
	}
	if material, err := checkedMaterialDigest(Candidate{}, StoredManifest{}, ApprovedBaseline{Baseline: invalidBehavior}); !errors.Is(err, ErrIntegrity) || material != "" {
		t.Fatalf("material identity contract accepted invalid canonical text: %q %v", material, err)
	}
}

func TestTicket80ApprovedImportPublicationContractRejectsReadOnlyLedger(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	pending := f.preview(t)
	rows, diagnostics := ParseResultsCSV(f.pending)
	if len(diagnostics) != 0 || len(rows) == 0 || len(rows) != pending.Rows {
		t.Fatalf("fixture lacks its real parsed pending import: parsed%d preview%d issues%v", len(rows), pending.Rows, diagnostics)
	}
	err := f.app.db.View(func(tx *bolt.Tx) error {
		plan, err := buildImportPlan(tx, rows, f.at, "DEMO")
		if err != nil || len(plan.issues) != 0 || len(plan.newRuns) != 1 || plan.ledgerHighWater != 2 {
			t.Fatalf("fixture lacks a valid new ledger entry: %+v %v", plan, err)
		}
		binding, err := getJSON[previewBinding](tx, previewBucket, pending.Token)
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := f.app.persistImport(tx, plan, f.pending, binding, secondActor, f.at, len(rows))
		if !reflect.DeepEqual(receipt, Receipt{}) {
			t.Error("failed ledger write exposed a usable receipt")
		}
		return err
	})
	if !errors.Is(err, bolt.ErrTxNotWritable) {
		t.Fatalf("import publication contract suppressed native ledger refusal: %v", err)
	}
	if _, err := f.app.Receipt(secondActor, secondProject, "I-"+digest(f.pending)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed ledger write published a receipt: %v", err)
	}
	if _, body, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || !bytes.Equal(body, f.html) {
		t.Fatalf("failed ledger write changed historical report: %v", err)
	}
	receipt, err := f.app.CommitImport(secondActor, secondProject, pending.Token, nil)
	if err != nil || receipt.LedgerHighWater != 2 || len(receipt.AddedRuns) != 1 {
		t.Fatalf("valid commit after native refusal did not publish ledger2 exactly once: %+v %v", receipt, err)
	}
	if raw, err := f.app.SourceCSV(secondActor, secondProject, receipt.ID); err != nil || !bytes.Equal(raw, f.pending) {
		t.Fatalf("valid retry published different bytes: %v", err)
	}
}

func TestTicket80ApprovedRevisionAndFingerprintContractsPreserveCauses(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	_, _, approved, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	baseline := approved.Baseline
	baseline.ID = "UNCOMMITTED-CONTRACT"
	err = f.app.db.View(func(tx *bolt.Tx) error { return registerApprovedRevisions(tx, baseline, approved.Descriptor.Digest) })
	if !errors.Is(err, bolt.ErrTxNotWritable) {
		t.Fatalf("revision contract suppressed real native write error: %v", err)
	}
	err = f.app.db.View(func(tx *bolt.Tx) error { return registerImmutable(tx, testOwners, "UNENCODABLE", func() {}) })
	var unsupported *json.UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		t.Fatalf("immutable contract hid standard encoding cause: %v", err)
	}
	if err := f.app.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(baselineRevision)).Get([]byte(versionKey(baseline.ID, baseline.Revision))) != nil ||
			tx.Bucket([]byte(testOwners)).Get([]byte("UNENCODABLE")) != nil {
			t.Error("failed contract published an immutable identity")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTicket80ApprovedSnapshotClockContractDoesNotReadMissingMetadata(t *testing.T) {
	db, err := bolt.Open(filepath.Join(t.TempDir(), "clock-contract.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	clockCalls := 0
	app := &Board{clock: func() time.Time { clockCalls++; return time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC) }}
	if err := db.View(func(tx *bolt.Tx) error {
		if got := app.aiSnapshotTime(tx); !got.IsZero() || clockCalls != 0 {
			t.Fatalf("missing metadata consumed an AI assessment clock: %v calls%d", got, clockCalls)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTicket80ApprovedAccountTransactionsPreserveNativeRefusals(t *testing.T) {
	app, err := Open(filepath.Join(t.TempDir(), "account-contract.db"), Settings{ProjectID: "contracts", ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	record, err := makeAccount("owner", "synthetic-contract-password", []Role{Operator, Reviewer})
	if err != nil {
		t.Fatal(err)
	}
	err = app.db.View(func(tx *bolt.Tx) error { return app.bootstrapAccount(tx, record) })
	if !errors.Is(err, bolt.ErrTxNotWritable) {
		t.Fatalf("bootstrap contract suppressed native read-only error: %v", err)
	}
	if _, err := app.Account("owner"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refused bootstrap published an account: %v", err)
	}
	if err := app.db.Update(func(tx *bolt.Tx) error { return app.bootstrapAccount(tx, record) }); err != nil {
		t.Fatal(err)
	}
	if err := app.SetAccount("owner", "viewer", "synthetic-viewer-password", []Role{Viewer}); err != nil {
		t.Fatal(err)
	}
	err = app.db.View(func(tx *bolt.Tx) error { return app.removeAccount(tx, "owner", "viewer") })
	if !errors.Is(err, bolt.ErrTxNotWritable) {
		t.Fatalf("account removal contract suppressed native read-only error: %v", err)
	}
	if _, err := app.Authenticate("viewer", "synthetic-viewer-password"); err != nil {
		t.Fatalf("refused removal lost the existing account: %v", err)
	}
	if err := app.RemoveAccount("owner", "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Account("viewer"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("authorized removal did not finish after native refusal: %v", err)
	}
}

func TestTicket80ApprovedOperatingTransactionsRetainSettingsOnNativeRefusal(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	for _, name := range []string{"rate", "duration"} {
		t.Run(name, func(t *testing.T) {
			update := func(tx *bolt.Tx) ([]string, error) {
				return updateImportRate(tx, ImportRatePolicy{Count: 11, Window: time.Minute})
			}
			key := importRateKey
			if name == "duration" {
				key = operatingConfigKey
				update = func(tx *bolt.Tx) ([]string, error) {
					return updateOperatingDuration(tx, evidenceRetention, 181*24*time.Hour)
				}
			}
			for _, malformed := range []bool{false, true} {
				operation := func(tx *bolt.Tx) error {
					if malformed {
						if err := tx.Bucket([]byte(metaBucket)).Put([]byte(key), []byte("{")); err != nil {
							t.Fatal(err)
						}
					}
					refs, err := update(tx)
					if refs != nil {
						t.Error("failed settings contract returned success references")
					}
					return err
				}
				var err error
				want := error(bolt.ErrTxNotWritable)
				if malformed {
					want = ErrIntegrity
					err = f.app.db.Update(operation)
				} else {
					err = f.app.db.View(operation)
				}
				if !errors.Is(err, want) {
					t.Fatalf("operating contract suppressed real %s cause: %v", name, err)
				}
			}
		})
	}
	config, rate, err := f.app.OperatingConfigSnapshot(secondActor, secondProject)
	if err != nil || config[evidenceRetention] != 180*24*time.Hour || rate != (ImportRatePolicy{Count: 10, Window: time.Minute}) {
		t.Fatalf("native settings refusals changed store-wide policy: %v %v %v", config, rate, err)
	}
	if err := f.app.SetImportRate(secondActor, secondProject, ImportRatePolicy{Count: 11, Window: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.SetOperatingDuration(secondActor, secondProject, evidenceRetention, 181*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, body, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || !bytes.Equal(body, f.html) {
		t.Fatalf("settings contract changed historical evidence: %v", err)
	}
}

func TestTicket80ApprovedStoreIdentityTransactionKeepsNativeCause(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	err := f.app.db.View(func(tx *bolt.Tx) error { return f.app.writeStoreIdentity(tx.Bucket([]byte(metaBucket))) })
	if !errors.Is(err, bolt.ErrTxNotWritable) {
		t.Fatalf("identity publication suppressed a real native error: %v", err)
	}
	if err := f.app.db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(metaBucket))
		if string(meta.Get([]byte("project"))) != secondProject || string(meta.Get([]byte("product"))) != "DEMO" || string(meta.Get([]byte("schema"))) != "reb-store/1" {
			t.Error("refused identity publication changed the retained store identity")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTicket80ApprovedCandidatePublicationRejectsInvalidContractAndIndex(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	_, stored, approved, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	input := CandidateInput{Manifest: stored.Manifest, BaselineDigest: approved.Descriptor.Digest}
	bad := approved
	bad.Baseline.Behaviors = []Behavior{{Text: "\xff", Tests: []TestPlan{{ExpectedOutcome: "valid"}}}}
	err = f.app.db.View(func(tx *bolt.Tx) error {
		result, reused, err := f.app.publishCandidate(tx, secondActor, input, stored.Descriptor, bad, candidatePredecessor{})
		if !reflect.DeepEqual(result, Candidate{}) || reused {
			t.Error("invalid material contract exposed a usable candidate")
		}
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("candidate publication hid a real invalid encoding: %v", err)
	}
	err = f.app.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(materialBucket)).Put([]byte(f.candidate.MaterialDigest), []byte("{")); err != nil {
			t.Fatal(err)
		}
		result, reused, err := f.app.publishCandidate(tx, secondActor, input, stored.Descriptor, approved, candidatePredecessor{})
		if !reflect.DeepEqual(result, Candidate{}) || reused {
			t.Error("invalid native material index exposed a candidate")
		}
		return err
	})
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("candidate publication suppressed malformed native material index: %v", err)
	}
	if err := f.app.db.View(func(tx *bolt.Tx) error {
		result, reused, err := f.app.publishCandidate(tx, secondActor, input, stored.Descriptor, approved, candidatePredecessor{})
		if err != nil || !reused || !reflect.DeepEqual(result, f.candidate) {
			t.Fatalf("candidate retry did not reuse the original identity: %+v %t %v", result, reused, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if raw, err := f.app.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(raw, f.source) {
		t.Fatalf("refused candidate publication changed retained evidence: %v", err)
	}
}

func TestTicket80ApprovedFrozenPublicationRejectsInvalidEvidenceBeforeAllocation(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	assessment, err := f.app.Assess(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	assessment.Candidate.ManifestDigest = "\xff"
	err = f.app.db.View(func(tx *bolt.Tx) error {
		report, err := f.app.publishFrozenAssessment(tx, secondActor, assessment)
		if report != (Report{}) {
			t.Error("invalid evidence contract exposed a report identity")
		}
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("frozen publication hid invalid canonical evidence: %v", err)
	}
	if reports, err := f.app.Reports(secondActor, secondProject, f.candidate.ID); err != nil || len(reports) != 1 || reports[0] != f.report {
		t.Fatalf("invalid frozen evidence changed historical report inventory: %+v %v", reports, err)
	}
	next, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID)
	if err != nil || next.ID != "RPT-2" || next.Status != Sufficient {
		t.Fatalf("valid freeze after refusal lost allocation/status: %+v %v", next, err)
	}
	if _, body, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || !bytes.Equal(body, f.html) {
		t.Fatalf("frozen contract failure changed original report bytes: %v", err)
	}
}

func TestTicket80ApprovedImmutableContractRejectsUnencodableValueBeforeMutation(t *testing.T) {
	expected := map[string][]byte{"retained": []byte("original")}
	err := rememberImmutableIdentity(expected, "new", func() {})
	var unsupported *json.UnsupportedTypeError
	if !errors.As(err, &unsupported) || len(expected) != 1 || !bytes.Equal(expected["retained"], []byte("original")) {
		t.Fatalf("immutable reconstruction accepted unsupported encoding or mutated its ledger: %v %v", expected, err)
	}
}

func TestTicket80ApprovedCrossIndexContractsRejectInconsistentSnapshots(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	_, _, approved, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = f.app.db.View(func(tx *bolt.Tx) error {
		return verifyApprovedDraftLink(tx, approved, map[string]string{versionKey(approved.Baseline.ID, approved.Baseline.Revision): "wrong-canonical-digest"})
	})
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("approved link contract accepted a different revision digest: %v", err)
	}
	err = f.app.db.Update(func(tx *bolt.Tx) error {
		expected, _, err := expectedStoredRuns(tx, f.candidate.ID)
		if err != nil {
			t.Fatal(err)
		}
		run, err := getJSON[Run](tx, runBucket, f.receipt.AddedRuns[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Bucket([]byte(candidateBucket)).Delete([]byte(f.candidate.ID)); err != nil {
			t.Fatal(err)
		}
		return validateRunCustody(tx, run, expected[run.ID])
	})
	if !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "missing candidate") {
		t.Fatalf("run custody contract accepted a missing native candidate: %v", err)
	}
	if assessment, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); err != nil || assessment.Status != Sufficient || assessment.LedgerHighWater != 1 {
		t.Fatalf("refused cross-index contract changed live retained evidence: %+v %v", assessment, err)
	}
	err = f.app.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucket([]byte(aiHistoryBucket))
		if err != nil {
			t.Fatal(err)
		}
		if err := bucket.Put([]byte("retained-contract"), []byte("original AI bytes")); err != nil {
			t.Fatal(err)
		}
		return applyPurge(tx, purgeSelection{plan: PurgePlan{AIRequestIDs: []string{"retained-contract"}}}, f.at)
	})
	if !errors.Is(err, bolt.ErrBucketNotFound) {
		t.Fatalf("historical evidence purge hid a real missing binding store: %v", err)
	}
	if _, body, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || !bytes.Equal(body, f.html) {
		t.Fatalf("failed cross-index contract changed frozen evidence: %v", err)
	}
}

func TestTicket80ApprovedRevalidationTransactionKeepsHoldOnNativeRefusal(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
		t.Fatal(err)
	}
	err := f.app.db.View(func(tx *bolt.Tx) error { return f.app.revalidateTransaction(tx, secondActor, secondProject) })
	if !errors.Is(err, bolt.ErrTxNotWritable) {
		t.Fatalf("revalidation contract suppressed native hold deletion refusal: %v", err)
	}
	if held, err := f.app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
		t.Fatalf("failed revalidation released the assessment hold: %t %v", held, err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); err != nil {
		t.Fatal(err)
	}
	if held, err := f.app.AssessmentSuspended(secondActor, secondProject); err != nil || held {
		t.Fatalf("valid revalidation did not release its verified hold: %t %v", held, err)
	}
	if _, body, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || !bytes.Equal(body, f.html) {
		t.Fatalf("revalidation contract changed frozen evidence: %v", err)
	}
}

func TestTicket80ApprovedExpiryTransactionsKeepDurableIntentUntilNativeSuccess(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	dir := t.TempDir()
	backup, err := f.app.Backup(secondActor, secondProject, dir)
	if err != nil {
		t.Fatal(err)
	}
	intent := backupExpiry{Directory: dir, Name: filepath.Base(backup.Path), MinimumAge: defaultBackupExpiry}
	err = f.app.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(metaBucket)).Put([]byte(operatingConfigKey), []byte("{")); err != nil {
			t.Fatal(err)
		}
		age, pending, err := readBackupExpiryState(tx)
		if age != 0 || pending != nil {
			t.Error("invalid expiry settings exposed partial work")
		}
		return err
	})
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expiry state contract hid malformed configuration: %v", err)
	}
	err = f.app.db.View(func(tx *bolt.Tx) error { return f.app.publishBackupExpiryIntent(tx, secondActor, intent) })
	if !errors.Is(err, bolt.ErrTxNotWritable) {
		t.Fatalf("expiry intent contract hid native write refusal: %v", err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error { return f.app.publishBackupExpiryIntent(tx, secondActor, intent) }); err != nil {
		t.Fatal(err)
	}
	err = f.app.db.Update(func(tx *bolt.Tx) error {
		other := intent
		other.MinimumAge++
		if err := putJSON(tx, expiryBucket, intent.key(), other); err != nil {
			t.Fatal(err)
		}
		return f.app.finishExpiryTransaction(tx, secondActor, intent)
	})
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("expiry contract accepted a different recorded intent: %v", err)
	}
	for _, name := range []string{backup.Path, backup.Path + ".sha256"} {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("refused intent changed a verified backup: %v", err)
		}
	}
	path := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	app, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return f.at.Add(31 * 24 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	err = app.db.View(func(tx *bolt.Tx) error { return app.finishExpiryTransaction(tx, secondActor, intent) })
	if !errors.Is(err, bolt.ErrTxNotWritable) {
		t.Fatalf("expiry completion suppressed native journal deletion refusal: %v", err)
	}
	for _, name := range []string{backup.Path, backup.Path + ".sha256"} {
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expiry completion did not remove the verified pair before journal deletion: %s %v", name, err)
		}
	}
	if err := app.db.View(func(tx *bolt.Tx) error {
		stored, err := getJSON[backupExpiry](tx, expiryBucket, intent.key())
		if err != nil || stored != intent {
			t.Fatalf("native completion refusal lost its durable exact intent: %+v %v", stored, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count, err := app.ExpireBackups(secondActor, secondProject, dir); err != nil || count != 1 {
		t.Fatalf("verified retry did not reconcile completed filesystem removal: %d %v", count, err)
	}
	if err := app.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(expiryBucket)).Get([]byte(intent.key())) != nil {
			t.Error("successful expiry retry retained its completed intent")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, body, err := app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || !bytes.Equal(body, f.html) {
		t.Fatalf("expiry contract changed original report bytes: %v", err)
	}
}

func TestTicket80ApprovedThresholdContractRejectsMissingColdBaseline(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	work := thresholdWork{sharedDigest: f.candidate.BaselineDigest}
	err := f.app.db.View(func(tx *bolt.Tx) error {
		changes, err := work.appendCandidateChanges(tx, thresholdCandidate{id: f.candidate.ID, baselineDigest: "missing-cold-contract"}, nil)
		if changes != nil {
			t.Error("missing cold contract exposed reviewable threshold changes")
		}
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("threshold contract hid its native cold lookup error: %v", err)
	}
	if assessment, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); err != nil || assessment.Status != Sufficient {
		t.Fatalf("refused cold threshold contract changed retained assessment: %+v %v", assessment, err)
	}
}
