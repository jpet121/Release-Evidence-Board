package board

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestRF16PurgeRefusesDamagedSharedEvidenceAndFrozenOwnership(t *testing.T) {
	for _, test := range []struct {
		name, fault string
		held        bool
		direct      bool
	}{
		{"shared receipt while held", "receipt", true, false},
		{"fresh receipt fault at preview", "receipt", false, false},
		{"fresh receipt fault at confirmation", "receipt", false, true},
		{"shared report while held", "report", true, false},
		{"fresh report fault at confirmation", "report", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			b, baseline, start := reviewBoard(t)
			now := start
			b.clock = func() time.Time { return now }
			old := reviewCandidate(t, b, baseline, "a")
			now = now.Add(24 * time.Hour)
			retained := reviewCandidate(t, b, baseline, "b")
			var receipt Receipt
			var original []byte
			oldRun := reviewRows(old, "a", "OLD", "PASS", now)
			retainedRun := reviewRows(retained, "b", "RETAINED", "PASS", now)
			if test.fault == "receipt" {
				receipt, _, original = reviewCommit(t, b, old, oldRun+retainedRun)
			} else {
				reviewCommit(t, b, old, oldRun)
				receipt, _, original = reviewCommit(t, b, retained, retainedRun)
			}
			report, err := b.Freeze(secondActor, secondProject, retained.ID)
			if err != nil || report.Status != Sufficient {
				t.Fatalf("invalid report fixture: %s, %v", report.Status, err)
			}
			_, frozen, err := b.ReportHTML(secondActor, secondProject, report.ID)
			if err != nil {
				t.Fatal(err)
			}
			now = start.Add(180 * 24 * time.Hour)
			scope := []string{old.ID}
			healthyPlan, err := b.PreviewPurge(secondActor, secondProject, scope)
			if err != nil || healthyPlan.Confirmation == "" {
				t.Fatalf("healthy purge preview failed: %v", err)
			}
			if test.fault == "receipt" && !slices.Equal(healthyPlan.SharedBlockers, []string{retained.ID}) {
				t.Fatal("healthy preview did not protect the unexpired candidate")
			}
			if test.held {
				if err := b.SuspendAssessments(secondActor, secondProject); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.db.Update(func(tx *bolt.Tx) error {
				if test.fault == "receipt" {
					receipt.CandidateIDs = []string{old.ID}
					return putJSON(tx, receiptBucket, receipt.Hash, receipt)
				}
				report.CandidateID = old.ID
				return putJSON(tx, reportBucket, report.ID, report)
			}); err != nil {
				t.Fatal(err)
			}
			confirmation := healthyPlan.Confirmation
			if !test.direct {
				plan, err := b.PreviewPurge(secondActor, secondProject, scope)
				if !errors.Is(err, ErrIntegrity) || plan.Confirmation != "" {
					t.Errorf("damaged scope had usable purge preview: blockers=%v, err=%v", plan.SharedBlockers, err)
					if plan.Confirmation != "" {
						confirmation = plan.Confirmation
					}
				}
			}
			if plan, err := b.Purge(secondActor, secondProject, scope, confirmation); !errors.Is(err, ErrIntegrity) || plan.Confirmation != "" {
				t.Errorf("damaged scope succeeded or returned a usable purge plan: %v", err)
			}
			if err := b.db.View(func(tx *bolt.Tx) error {
				for _, entry := range []struct{ bucket, key string }{
					{receiptBucket, receipt.Hash}, {reportBucket, report.ID}, {reportMetaBucket, report.ID},
					{runBucket, "RETAINED"}, {candidateBucket, old.ID}, {candidateBucket, retained.ID},
				} {
					if tx.Bucket([]byte(entry.bucket)).Get([]byte(entry.key)) == nil {
						t.Errorf("failed purge removed a retained record from %s", entry.bucket)
					}
				}
				if !bytes.Equal(tx.Bucket([]byte(csvBucket)).Get([]byte(receipt.Hash)), original) ||
					!bytes.Equal(tx.Bucket([]byte(htmlBucket)).Get([]byte(report.ID)), frozen) {
					t.Error("failed purge removed or rewrote protected source evidence")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if held, err := b.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
				t.Errorf("purge detected an ownership fault but did not suspend: held=%t, err=%v", held, err)
			}
			assertNoCustodySuccess(t, b, "retention.purge")
		})
	}
}

func TestRF17CandidateReuseRejectsMissingOrRedirectedMaterialIndex(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		name := "missing"
		if redirect {
			name = "redirected"
		}
		t.Run(name, func(t *testing.T) {
			b, baseline, at := reviewBoard(t)
			failed := reviewCandidate(t, b, baseline, "a")
			passing := reviewCandidate(t, b, baseline, "b")
			reviewCommit(t, b, failed, reviewRows(failed, "a", "FAILED", "FAIL", at))
			reviewCommit(t, b, passing, reviewRows(passing, "b", "PASSING", "PASS", at))
			before, err := b.Assess(secondActor, secondProject, failed.ID)
			if err != nil || before.Status != Blocked || before.Behaviors[0].Status != Fail {
				t.Fatalf("fixture does not have a definitive failure: %s, %v", before.Status, err)
			}
			_, manifest, _, err := b.Candidate(secondActor, secondProject, failed.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.db.Update(func(tx *bolt.Tx) error {
				if redirect {
					return putJSON(tx, materialBucket, failed.MaterialDigest, passing.ID)
				}
				return tx.Bucket([]byte(materialBucket)).Delete([]byte(failed.MaterialDigest))
			}); err != nil {
				t.Fatal(err)
			}
			manifest.Manifest.ReleaseID, manifest.Manifest.ApplicationVersion = "release-renamed", "perceived new release"
			input := CandidateInput{Manifest: manifest.Manifest, BaselineDigest: baseline.Descriptor.Digest}
			labelled, reused, err := b.CreateCandidate(secondActor, secondProject, input)
			if err == nil {
				if !reused {
					reviewCommit(t, b, labelled, reviewRows(labelled, "renamed", "BYPASS", "PASS", at))
				}
				board, assessmentErr := b.Assess(secondActor, secondProject, labelled.ID)
				report, reportErr := b.Freeze(secondActor, secondProject, labelled.ID)
				t.Errorf("damaged index changed effective candidate: reused=%t predecessor=%q changes=%d assessment=%s (%v) report=%s (%v)",
					reused, labelled.PredecessorID, len(labelled.Changes), board.Status, assessmentErr, report.Status, reportErr)
			} else if !errors.Is(err, ErrIntegrity) || labelled.ID != "" || reused {
				t.Errorf("expected fail-closed identity lookup: %#v, %t, %v", labelled, reused, err)
			}
			if held, err := b.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
				t.Errorf("material index fault did not suspend: held=%t, err=%v", held, err)
			}
			if a, err := b.Assess(secondActor, secondProject, passing.ID); !errors.Is(err, ErrIntegrity) || a.Status != "" {
				t.Errorf("known catalog corruption allowed a second assessment: %s, %v", a.Status, err)
			}
			var count int
			if err := b.db.View(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(candidateBucket)).ForEach(func(_, _ []byte) error { count++; return nil })
			}); err != nil || count != 2 {
				t.Errorf("label-only change mutated candidate inventory: count=%d, err=%v", count, err)
			}
		})
	}
}

func TestRF17ApprovalRejectsCorruptedImmutableCatalog(t *testing.T) {
	for _, bucket := range []string{baselineRevision, behaviorRevision, policyRevision, testOwners} {
		t.Run(bucket, func(t *testing.T) {
			b, baseline, _ := reviewBoard(t)
			contents := baseline.Baseline
			key := versionKey(contents.ID, contents.Revision)
			if bucket != baselineRevision {
				contents.ID = "copy"
			}
			behavior := &contents.Behaviors[0]
			switch bucket {
			case behaviorRevision:
				key = versionKey(behavior.ID, behavior.Revision)
				behavior.Text = "Different contract under a protected revision"
			case policyRevision:
				key = versionKey(behavior.Policy.ID, behavior.Policy.Revision)
				behavior.Policy.MaxAgeHours++
			case testOwners:
				key = behavior.Tests[0].ID
				behavior.ID = "OtherOwner"
				behavior.Tests[1].ID = "OtherTest"
			}
			draft, err := b.CreateDraft(secondActor, secondProject, contents, DraftOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(bucket)).Delete([]byte(key))
			}); err != nil {
				t.Fatal(err)
			}
			if approval, err := b.Approve(secondActor, secondProject, draft.ID, draft.Revision); !errors.Is(err, ErrIntegrity) || approval.Descriptor.Digest != "" {
				t.Fatalf("approval consulted unverified immutable catalog %s: %v", bucket, err)
			}
		})
	}
}

func TestRF17AssessmentAndImportCheckMaterialLinkBeforeNewEvidence(t *testing.T) {
	for _, operation := range []string{"assess", "freeze", "preview"} {
		t.Run(operation, func(t *testing.T) {
			b, baseline, at := reviewBoard(t)
			candidate := reviewCandidate(t, b, baseline, "a")
			reviewCommit(t, b, candidate, reviewRows(candidate, "a", "PASSING", "PASS", at))
			if err := b.db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(materialBucket)).Delete([]byte(candidate.MaterialDigest))
			}); err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "assess":
				state, err := b.Assess(secondActor, secondProject, candidate.ID)
				if !errors.Is(err, ErrIntegrity) || state.Status != "" {
					t.Errorf("unverified material link produced an assessment: %s, %v", state.Status, err)
				}
			case "freeze":
				report, err := b.Freeze(secondActor, secondProject, candidate.ID)
				if !errors.Is(err, ErrIntegrity) || report.ID != "" {
					t.Errorf("unverified material link produced a report: %s, %v", report.Status, err)
				}
			case "preview":
				raw := []byte(strings.Join(resultColumns[:], ",") + "\n" + reviewRows(candidate, "a", "SECOND", "PASS", at))
				preview, err := b.PreviewImport(secondActor, secondProject, candidate.ID, raw)
				if !errors.Is(err, ErrIntegrity) || preview.Token != "" {
					t.Errorf("unverified material link admitted an import preview: %v", err)
				}
			}
			if held, err := b.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
				t.Errorf("fault did not latch the hold: held=%t, err=%v", held, err)
			}
			kept, _, _, err := b.Candidate(secondActor, secondProject, candidate.ID)
			if err != nil || kept.ID != candidate.ID {
				t.Errorf("hold concealed intact canonical provenance: %v", err)
			}
		})
	}
}

func TestRF16BackupExpiryRefusesRedirectedJournal(t *testing.T) {
	b, _, start := reviewBoard(t)
	now := start
	b.clock = func() time.Time { return now }
	directory := t.TempDir()
	old, err := b.Backup(secondActor, secondProject, directory)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * 24 * time.Hour)
	newest, err := b.Backup(secondActor, secondProject, directory)
	if err != nil {
		t.Fatal(err)
	}
	oldIntent := backupExpiry{Directory: directory, Name: filepath.Base(old.Path)}
	redirected := backupExpiry{Directory: directory, Name: filepath.Base(newest.Path)}
	if err := b.db.Update(func(tx *bolt.Tx) error {
		return putJSON(tx, expiryBucket, oldIntent.key(), redirected)
	}); err != nil {
		t.Fatal(err)
	}
	if count, err := b.ExpireBackups(secondActor, secondProject, directory); !errors.Is(err, ErrIntegrity) || count != 0 {
		t.Errorf("altered expiry journal claimed deletion: count=%d, err=%v", count, err)
	}
	for _, path := range []string{old.Path, old.Path + ".sha256", newest.Path, newest.Path + ".sha256"} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("unverified journal removed a backup or its checksum: %v", err)
		}
	}
	assertNoCustodySuccess(t, b, "backup.expire")
}

func TestRF16BackupExpiryChecksAgeAndHoldBeforePhysicalDeletion(t *testing.T) {
	b, _, start := reviewBoard(t)
	now := start
	b.clock = func() time.Time { return now }
	directory := t.TempDir()
	backup, err := b.Backup(secondActor, secondProject, directory)
	if err != nil {
		t.Fatal(err)
	}
	intent := backupExpiry{Directory: directory, Name: filepath.Base(backup.Path)}
	if err := b.db.Update(func(tx *bolt.Tx) error { return putJSON(tx, expiryBucket, intent.key(), intent) }); err != nil {
		t.Fatal(err)
	}
	check := func(want error) {
		t.Helper()
		if count, err := b.ExpireBackups(secondActor, secondProject, directory); count != 0 || !errors.Is(err, want) {
			t.Errorf("expiry returned %d, %v; expected zero removals and %v", count, err, want)
		}
		for _, path := range []string{backup.Path, backup.Path + ".sha256"} {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("expiry discarded a protected backup companion: %v", err)
			}
		}
	}
	check(ErrInvalid)
	now = start.Add(30 * 24 * time.Hour)
	if err := b.SuspendAssessments(secondActor, secondProject); err != nil {
		t.Fatal(err)
	}
	check(ErrIntegrity)
	assertNoCustodySuccess(t, b, "backup.expire")
	if err := b.Revalidate(secondActor, secondProject); err != nil {
		t.Fatal(err)
	}
	if count, err := b.ExpireBackups(secondActor, secondProject, directory); count != 1 || err != nil {
		t.Errorf("sound old backup did not expire after reviewer revalidation: %d, %v", count, err)
	}
}

func TestRF16PurgeRefusesCorruptedEligibilityMetadata(t *testing.T) {
	for _, fault := range []string{
		"candidate time", "audit time", "audit sequence", "candidate binding missing", "candidate binding changed",
		"audit binding missing", "audit binding changed",
	} {
		t.Run(fault, func(t *testing.T) {
			b, baseline, start := reviewBoard(t)
			now := start
			b.clock = func() time.Time { return now }
			old := reviewCandidate(t, b, baseline, "a")
			now = start.Add(179 * 24 * time.Hour)
			retained := reviewCandidate(t, b, baseline, "b")
			receipt, _, original := reviewCommit(t, b, retained, reviewRows(retained, "b", "RECENT", "PASS", now))
			events, err := b.Audit(secondActor, secondProject)
			if err != nil {
				t.Fatal(err)
			}
			oldAudit := events[0]
			recentAudit := events[len(events)-1]
			recentKey := fmt.Sprintf("%020d", recentAudit.Sequence)
			now = start.Add(180 * 24 * time.Hour)
			scope := []string{old.ID}
			if err := b.db.Update(func(tx *bolt.Tx) error {
				switch fault {
				case "candidate time":
					retained.CreatedAt = start
					scope = append(scope, retained.ID)
					return putJSON(tx, candidateBucket, retained.ID, retained)
				case "audit time":
					recentAudit.Time = start
					return putJSON(tx, auditBucket, recentKey, recentAudit)
				case "candidate binding missing":
					return tx.Bucket([]byte(candidateMetaBucket)).Delete([]byte(retained.ID))
				case "candidate binding changed":
					return tx.Bucket([]byte(candidateMetaBucket)).Put([]byte(retained.ID), []byte(digest([]byte("other record"))))
				case "audit binding missing":
					return tx.Bucket([]byte(auditMetaBucket)).Delete([]byte(recentKey))
				case "audit binding changed":
					return tx.Bucket([]byte(auditMetaBucket)).Put([]byte(recentKey), []byte(digest([]byte("other event"))))
				default:
					key := fmt.Sprintf("%020d", oldAudit.Sequence)
					oldAudit.Sequence = recentAudit.Sequence
					return putJSON(tx, auditBucket, key, oldAudit)
				}
			}); err != nil {
				t.Fatal(err)
			}
			plan, err := b.PreviewPurge(secondActor, secondProject, scope)
			if !errors.Is(err, ErrIntegrity) || plan.Confirmation != "" {
				t.Errorf("corrupted expiry metadata produced a usable plan: %v", err)
			}
			if _, err := b.Purge(secondActor, secondProject, scope, plan.Confirmation); !errors.Is(err, ErrIntegrity) {
				t.Errorf("corrupted expiry metadata permitted purge: %v", err)
			}
			if err := b.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
				t.Errorf("unverified eligibility information passed reviewer revalidation: %v", err)
			}
			if err := b.db.View(func(tx *bolt.Tx) error {
				if !bytes.Equal(tx.Bucket([]byte(csvBucket)).Get([]byte(receipt.Hash)), original) ||
					tx.Bucket([]byte(auditBucket)).Get([]byte(recentKey)) == nil {
					t.Error("purge removed unexpired records based on corrupted eligibility")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRF16PurgePreviewUsesConsistentReadDuringDraftWrites(t *testing.T) {
	b, baseline, start := reviewBoard(t)
	old := reviewCandidate(t, b, baseline, "a")
	b.clock = func() time.Time { return start.Add(180 * 24 * time.Hour) }
	begin := make(chan struct{})
	finished := make(chan error, 2)
	go func() {
		<-begin
		for i := 0; i < 80; i++ {
			_, err := b.CreateDraft(secondActor, secondProject, Baseline{ProductID: "DEMO", ID: fmt.Sprintf("new-%d", i)}, DraftOptions{})
			if err != nil {
				finished <- err
				return
			}
		}
		finished <- nil
	}()
	go func() {
		<-begin
		for range 80 {
			plan, err := b.PreviewPurge(secondActor, secondProject, []string{old.ID})
			if err != nil || plan.Confirmation == "" || len(plan.Ineligible) != 0 {
				finished <- fmt.Errorf("consistent eligible preview failed: %v", err)
				return
			}
		}
		finished <- nil
	}()
	close(begin)
	for range 2 {
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}
}

func assertNoCustodySuccess(t *testing.T, b *Board, action string) {
	t.Helper()
	items, err := b.Audit(secondActor, secondProject)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(items, func(item AuditEvent) bool { return item.Action == action && item.Outcome == "success" }) {
		t.Errorf("failed custody operation produced a success audit for %s", action)
	}
}
