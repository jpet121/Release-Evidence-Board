package board

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupWholeStoreChecksDraftIdentityAndImmutableApprovalLinks(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{"D-without-number", "draft allocator and retained draft identifiers are inconsistent"},
		{"D-0", "draft allocator and retained draft identifiers are inconsistent"},
		{"D-01", "draft allocator and retained draft identifiers are inconsistent"},
		{"D-999", "draft allocator and retained draft identifiers are inconsistent"},
		{"invalid_approved_roster", "approved draft differs from its original descriptor or revision"},
		{"different_approved_digest", "approved draft differs from its original descriptor or revision"},
		{"duplicate_approved_revision", "approved draft differs from its original descriptor or revision"},
		{"conflicting_immutable_revision", "approved drafts disagree on an immutable identity"},
		{"empty_revision_index", "baseline revision index is inconsistent"},
		{"orphan_policy_identity", "immutable identity index differs from approved drafts"},
		{"missing_source_draft", "approved baseline lacks the matching source draft and revision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			_, _, approved, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			origin, err := f.app.Draft(secondActor, secondProject, approved.DraftID)
			if err != nil || origin.ApprovedDigest != approved.Descriptor.Digest {
				t.Fatalf("fixture lacks the exact original approved draft: %v", err)
			}
			pending, err := f.app.CreateDraft(secondActor, secondProject, approved.Baseline, DraftOptions{FromBaseline: approved.Descriptor.Digest})
			if err != nil || pending.ApprovedDigest != "" {
				t.Fatalf("fixture lacks a distinct valid pending draft: %v", err)
			}
			if err := f.app.Revalidate(secondActor, secondProject); err != nil {
				t.Fatalf("ordinary retained approval and pending draft do not revalidate: %v", err)
			}
			if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			err = f.app.db.Update(func(tx *bolt.Tx) error {
				if strings.HasPrefix(tc.name, "D-") {
					draft := pending
					draft.ID = tc.name
					return putBoundJSON(tx, draftBucket, draftMetaBucket, draft.ID, draft)
				}
				draft := origin
				switch tc.name {
				case "invalid_approved_roster":
					draft.Baseline.Behaviors = slices.Clone(draft.Baseline.Behaviors)
					draft.Baseline.Behaviors[0].Policy.MinPassBasisPoints = 10001
				case "different_approved_digest":
					draft.ApprovedDigest = strings.Repeat("f", 64)
				case "duplicate_approved_revision":
					draft.ID = pending.ID
				case "conflicting_immutable_revision":
					draft.ID = pending.ID
					draft.Baseline.Revision = pending.Baseline.Revision
					draft.Baseline.Behaviors = slices.Clone(draft.Baseline.Behaviors)
					draft.Baseline.Behaviors[0].Text = "Different text at the identical immutable behavior revision"
					descriptor, err := draft.Baseline.Descriptor()
					if err != nil {
						return err
					}
					draft.ApprovedDigest = descriptor.Digest
				case "empty_revision_index":
					return putJSON(tx, baselineRevision, versionKey(approved.Baseline.ID, approved.Baseline.Revision), "")
				case "orphan_policy_identity":
					return tx.Bucket([]byte(policyRevision)).Put([]byte("no-approved-owner:1"), []byte("independent orphan identity"))
				case "missing_source_draft":
					approved.DraftID = "D-700"
					return putBoundJSON(tx, baselineBucket, baselineMetaBucket, approved.Descriptor.Digest, approved)
				}
				return putBoundJSON(tx, draftBucket, draftMetaBucket, draft.ID, draft)
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("whole-store revalidation did not detect the exact draft or approval index fault: %v", err)
			}
			if assessment, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || assessment.Status != "" {
				t.Fatalf("approval catalog fault cleared the durable hold: %q %v", assessment.Status, err)
			}
			if raw, err := f.app.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(raw, f.source) {
				t.Fatalf("approval catalog refusal changed the previous original upload: %v", err)
			}
			if report, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(html, f.html) {
				t.Fatalf("approval catalog refusal changed the prior frozen artifact: %v", err)
			}
		})
	}
}

func TestFollowupWholeStoreChecksDistinctCandidateMaterialRecords(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{"wrong_retained_material", "candidate material identity is inconsistent"},
		{"duplicate_canonical_material", "candidate material identity is inconsistent"},
		{"empty_material_target", "candidate material index is inconsistent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			candidate, manifest, baseline, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			err = f.app.db.Update(func(tx *bolt.Tx) error {
				switch tc.name {
				case "wrong_retained_material":
					candidate.MaterialDigest = strings.Repeat("f", 64)
				case "duplicate_canonical_material":
					manifest.Manifest.ReleaseID = "another-retained-release-label"
					descriptor, err := manifest.Manifest.Descriptor()
					if err != nil {
						return err
					}
					manifest.Descriptor = descriptor
					candidate.ManifestDigest = descriptor.Digest
					candidate.ID = "C-" + digest([]byte(descriptor.Digest+baseline.Descriptor.Digest))
					if err := putJSON(tx, manifestBucket, descriptor.Digest, manifest); err != nil {
						return err
					}
				case "empty_material_target":
					return putJSON(tx, materialBucket, candidate.MaterialDigest, "")
				}
				return putBoundJSON(tx, candidateBucket, candidateMetaBucket, candidate.ID, candidate)
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("reviewer did not refuse the exact candidate material conflict: %v", err)
			}
			if report, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
				t.Fatalf("unverified material conflict exposed a new frozen decision: %q %v", report.ID, err)
			}
			if report, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(html, f.html) {
				t.Fatalf("material refusal changed the earlier frozen artifact: %v", err)
			}
		})
	}
}
