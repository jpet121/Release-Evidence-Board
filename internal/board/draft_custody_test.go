package board

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

const draftAuthor = "reviewer-c"

func retainedHighMarkAndReducedDraft(t *testing.T) (*Board, Candidate, Draft, time.Time) {
	t.Helper()
	b, baseline, at := reviewBoard(t)
	if err := b.SetAccount(secondActor, draftAuthor, "synthetic-independent-password", []Role{Reviewer}); err != nil {
		t.Fatal(err)
	}
	prior := reviewCandidate(t, b, baseline, "a")
	reviewCommit(t, b, prior, reviewRows(prior, "a", "HIGH", "FAIL", at))
	state, err := b.Assess(secondActor, secondProject, prior.ID)
	if err != nil || state.Status != Blocked || state.Behaviors[0].Status != Fail {
		t.Fatalf("original 100%% threshold did not produce a definitive failure: %s, %v", state.Status, err)
	}
	draft, err := b.CreateDraft(draftAuthor, secondProject, baseline.Baseline, DraftOptions{FromBaseline: baseline.Descriptor.Digest})
	if err != nil {
		t.Fatal(err)
	}
	contents := draft.Baseline
	contents.Behaviors = slices.Clone(contents.Behaviors)
	contents.Behaviors[0].Policy.MinPassBasisPoints = 5000
	draft, err = b.SaveDraft(secondActor, secondProject, draft.ID, draft.Revision, contents)
	if err != nil || draft.Author != draftAuthor || draft.Revision != 2 {
		t.Fatalf("reviewer A could not save C's pending draft: %v", err)
	}
	changes, err := b.ReviewThresholds(secondActor, secondProject, draft.ID)
	if err != nil || len(changes) != 1 || changes[0].Before != 10000 || changes[0].After != 5000 ||
		!slices.Equal(changes[0].ReducingActors, []string{secondActor}) {
		t.Fatalf("intact revision did not identify the actual reducing reviewer: %#v, %v", changes, err)
	}
	return b, prior, draft, at
}

func freezeHalfPassingSuccessor(t *testing.T, b *Board, prior Candidate, approved ApprovedBaseline, at time.Time) Report {
	t.Helper()
	successor := reviewCandidate(t, b, approved, "a")
	if successor.ID == prior.ID || successor.PredecessorID != prior.ID {
		t.Fatal("approved lower contract did not create a distinct successor")
	}
	rows := reviewRows(successor, "a", "LOW", "FAIL", at)
	rows = strings.ReplaceAll(rows, ",BH,1,P1,1,", ",BH,1,P1,2,")
	reviewCommit(t, b, successor, rows)
	state, err := b.Assess(secondActor, secondProject, successor.ID)
	if err != nil || state.Status != Sufficient || state.Behaviors[0].Passed != 1 || state.Behaviors[0].Planned != 2 {
		t.Fatalf("lower-mark successor did not evaluate exact half-passing evidence: %s, %v", state.Status, err)
	}
	report, err := b.Freeze(secondActor, secondProject, successor.ID)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestRF18RemovedThresholdEditorCannotApproveAndFreeze(t *testing.T) {
	b, prior, draft, at := retainedHighMarkAndReducedDraft(t)
	if _, err := b.Approve(secondActor, secondProject, draft.ID, draft.Revision); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reducing reviewer bypassed intact actor history: %v", err)
	}
	draft.ThresholdEdits = nil
	if err := b.db.Update(func(tx *bolt.Tx) error { return putJSON(tx, draftBucket, draft.ID, draft) }); err != nil {
		t.Fatal(err)
	}
	if err := b.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
		t.Errorf("changed pending actor history passed full revalidation: %v", err)
	}
	approval, err := b.Approve(secondActor, secondProject, draft.ID, draft.Revision)
	if err == nil {
		report := freezeHalfPassingSuccessor(t, b, prior, approval, at)
		t.Errorf("lost editor history allowed reducer %q to approve draft by %q and freeze a %s report", approval.Approver, approval.Author, report.Status)
	} else if !errors.Is(err, ErrIntegrity) || approval.Descriptor.Digest != "" {
		t.Errorf("approval did not fail closed on damaged actor history: %v", err)
	}
	if held, err := b.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
		t.Errorf("actor-history fault did not suspend assessment: %t, %v", held, err)
	}
}

func TestRF18IntactDraftCopyPreservesIndependentReviewAndRecovery(t *testing.T) {
	b, prior, draft, at := retainedHighMarkAndReducedDraft(t)
	copyContract := draft.Baseline
	copyContract.ID, copyContract.Revision = "independent-copy", 1
	copyDraft, err := b.CreateDraft(draftAuthor, secondProject, copyContract, DraftOptions{FromDraft: draft.ID})
	if err != nil || copyDraft.ParentDraft != draft.ID {
		t.Fatalf("copy did not bind its source draft: %v", err)
	}
	changes, err := b.ReviewThresholds(draftAuthor, secondProject, copyDraft.ID)
	if err != nil || len(changes) != 1 || !slices.Equal(changes[0].ReducingActors, []string{secondActor}) {
		t.Fatalf("draft copy lost earlier editor provenance: %#v, %v", changes, err)
	}
	if _, err := b.Approve(secondActor, secondProject, copyDraft.ID, copyDraft.Revision); !errors.Is(err, ErrForbidden) {
		t.Fatalf("copy allowed the reducing reviewer to approve: %v", err)
	}
	approved, err := b.Approve(draftAuthor, secondProject, copyDraft.ID, copyDraft.Revision)
	if err != nil || approved.Approver != draftAuthor || approved.DraftID != copyDraft.ID {
		t.Fatalf("independent reviewer could not approve the exact copied revision: %v", err)
	}
	report := freezeHalfPassingSuccessor(t, b, prior, approved, at)
	if report.Status != Sufficient {
		t.Fatalf("independently approved successor snapshot = %s", report.Status)
	}
	if err := b.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("genuine draft history no longer verifies after approval: %v", err)
	}
	if _, err := b.Backup(secondActor, secondProject, t.TempDir()); err != nil {
		t.Fatalf("valid approved and pending drafts blocked verified backup: %v", err)
	}
	b.clock = func() time.Time { return at.Add(180 * 24 * time.Hour) }
	scope := []string{prior.ID, report.CandidateID}
	preview, err := b.PreviewPurge(secondActor, secondProject, scope)
	if err != nil {
		t.Fatalf("healthy custody preview failed: %v", err)
	}
	if _, err := b.Purge(secondActor, secondProject, scope, preview.Confirmation); err != nil {
		t.Fatalf("approved draft custody blocked eligible candidate purge: %v", err)
	}
	retained, err := b.Draft(secondActor, secondProject, copyDraft.ID)
	if err != nil || retained.ApprovedDigest != approved.Descriptor.Digest ||
		!slices.Equal(retained.ThresholdEdits, copyDraft.ThresholdEdits) {
		t.Fatalf("candidate purge changed retained draft history: %v", err)
	}
	if err := b.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("purged descriptors broke retained approval history: %v", err)
	}
}

func TestRF18RejectsMissingChangedAndOrphanedDraftBindings(t *testing.T) {
	for _, fault := range []string{"missing binding", "wrong binding", "orphaned binding", "wrong primary identity"} {
		t.Run(fault, func(t *testing.T) {
			b, _, draft, _ := retainedHighMarkAndReducedDraft(t)
			if err := b.db.Update(func(tx *bolt.Tx) error {
				key := []byte(draft.ID)
				metadata := tx.Bucket([]byte(draftMetaBucket))
				switch fault {
				case "missing binding":
					return metadata.Delete(key)
				case "wrong binding":
					return metadata.Put(key, []byte(digest([]byte("other draft"))))
				case "orphaned binding":
					return tx.Bucket([]byte(draftBucket)).Delete(key)
				default:
					body := draft
					body.ID = "D-99"
					return putBoundJSON(tx, draftBucket, draftMetaBucket, draft.ID, body)
				}
			}); err != nil {
				t.Fatal(err)
			}
			if returned, err := b.Draft(secondActor, secondProject, draft.ID); !errors.Is(err, ErrIntegrity) || returned.ID != "" {
				t.Errorf("unverified pending draft was exposed: %q, %v", returned.ID, err)
			}
			if err := b.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
				t.Errorf("invalid draft custody passed full revalidation: %v", err)
			}
			if _, err := b.Approve(draftAuthor, secondProject, draft.ID, draft.Revision); !errors.Is(err, ErrIntegrity) {
				t.Errorf("independent reviewer could approve unbound draft: %v", err)
			}
		})
	}
}

func TestRF18CorruptedDraftHistoryCannotBeCopiedResealedOrListed(t *testing.T) {
	for _, operation := range []string{"approve", "edit", "copy", "threshold review", "catalog"} {
		t.Run(operation, func(t *testing.T) {
			b, _, draft, _ := retainedHighMarkAndReducedDraft(t)
			draft.ThresholdEdits = nil
			if err := b.db.Update(func(tx *bolt.Tx) error { return putJSON(tx, draftBucket, draft.ID, draft) }); err != nil {
				t.Fatal(err)
			}
			var err error
			switch operation {
			case "approve":
				_, err = b.Approve(secondActor, secondProject, draft.ID, draft.Revision)
			case "edit":
				_, err = b.SaveDraft(secondActor, secondProject, draft.ID, draft.Revision, draft.Baseline)
			case "copy":
				_, err = b.CreateDraft(draftAuthor, secondProject, draft.Baseline, DraftOptions{FromDraft: draft.ID})
			case "threshold review":
				_, err = b.ReviewThresholds(secondActor, secondProject, draft.ID)
			default:
				_, _, err = b.Baselines(secondActor, secondProject)
			}
			if !errors.Is(err, ErrIntegrity) {
				t.Errorf("%s consumed unchecked actor history: %v", operation, err)
			}
			if held, err := b.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
				t.Errorf("%s failed to hold new assessments: %t, %v", operation, held, err)
			}
		})
	}
}

func TestRF18DraftCreationCannotReplaceOrphanedBinding(t *testing.T) {
	b, baseline, _ := reviewBoard(t)
	const nextID = "D-2"
	fingerprint := []byte(digest([]byte("unowned draft metadata")))
	if err := b.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(draftMetaBucket)).Put([]byte(nextID), fingerprint)
	}); err != nil {
		t.Fatal(err)
	}
	contents := baseline.Baseline
	contents.ID = "new-draft"
	if _, err := b.CreateDraft(secondActor, secondProject, contents, DraftOptions{}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("creation resealed an orphaned companion: %v", err)
	}
	if err := b.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(draftBucket)).Get([]byte(nextID)) != nil ||
			string(tx.Bucket([]byte(draftMetaBucket)).Get([]byte(nextID))) != string(fingerprint) {
			t.Error("rejected creation rewrote an inconsistent draft slot")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRF18RestoreRefusesUnboundDraftHistoryDespiteTransportChecksum(t *testing.T) {
	for _, fault := range []string{"edited actor history", "legacy missing draft bindings"} {
		t.Run(fault, func(t *testing.T) {
			b, _, draft, at := retainedHighMarkAndReducedDraft(t)
			backup, err := b.Backup(secondActor, secondProject, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			wasClosed, err := bolt.Open(backup.Path, 0600, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = wasClosed.Update(func(tx *bolt.Tx) error {
				if fault == "legacy missing draft bindings" {
					return tx.DeleteBucket([]byte(draftMetaBucket))
				}
				draft.ThresholdEdits = nil
				return putJSON(tx, draftBucket, draft.ID, draft)
			})
			if err := errors.Join(err, wasClosed.Close()); err != nil {
				t.Fatal(err)
			}
			bytes, err := os.ReadFile(backup.Path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(backup.Path+".sha256", []byte(digest(bytes)+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "restore.db")
			settings := Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return at }}
			recovery, err := RestoreBackup(backup.Path, path, settings)
			if err != nil || recovery.IntegrityOK {
				t.Fatalf("restore certified unbound draft history: %t, %v", recovery.IntegrityOK, err)
			}
			restored, err := Open(path, settings)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if _, err := restored.Draft(secondActor, secondProject, draft.ID); !errors.Is(err, ErrIntegrity) {
				t.Errorf("recovery endorsed unchecked draft metadata: %v", err)
			}
			if err := restored.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
				t.Errorf("operator restored unauthorized editable state: %v", err)
			}
		})
	}
}
