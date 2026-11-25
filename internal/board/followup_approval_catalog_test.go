package board

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupCatalogFaultsKeepReviewerHoldUntilRepaired(t *testing.T) {
	cases := []struct {
		name, section, key, expected string
		remove                       bool
	}{
		{"wrong revision link", baselineRevision, "B1:1", "baseline revision index is inconsistent", false},
		{"missing revision link", baselineRevision, "B1:1", "baseline revision index omits approved drafts", true},
		{"wrong behavior fingerprint", behaviorRevision, "BH:1", "immutable identity index differs from approved drafts", false},
		{"missing policy fingerprint", policyRevision, "P1:1", "immutable identity index omits approved draft records", true},
		{"wrong roster owner", testOwners, "T1", "immutable identity index differs from approved drafts", false},
		{"different source draft", baselineBucket, "", "approved baseline lacks the matching source draft and revision", false},
		{"different approved author", baselineBucket, "", "approved baseline lacks the matching source draft and revision", false},
	}
	for _, entry := range cases {
		t.Run(entry.name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			_, _, original, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := f.app.CreateDraft(secondActor, secondProject, original.Baseline, DraftOptions{
				FromBaseline: original.Descriptor.Digest,
			})
			if err != nil || pending.ApprovedDigest != "" {
				t.Fatalf("fixture lacks a separate unapproved revision: %v", err)
			}
			if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			var originalIndex []byte
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				if entry.section == baselineBucket {
					// Preserve the canonical roster and its checksum. Vary only the
					// relationship to the independent, retained source draft.
					damaged := original
					if entry.name == "different source draft" {
						damaged.DraftID = pending.ID
					} else {
						damaged.Author = "different-author"
					}
					return putBoundJSON(tx, baselineBucket, baselineMetaBucket, original.Descriptor.Digest, damaged)
				}
				bucket := tx.Bucket([]byte(entry.section))
				key := []byte(entry.key)
				originalIndex = bytes.Clone(bucket.Get(key))
				if len(originalIndex) == 0 {
					return errors.New("fixture lacks the approved identity index entry")
				}
				if entry.remove {
					return bucket.Delete(key)
				}
				return bucket.Put(key, []byte(`"wrong-identity"`))
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), entry.expected) {
				t.Errorf("reviewer did not refuse the expected catalog fault %q: %v", entry.expected, err)
			}
			if assessment, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || assessment.Status != "" {
				t.Errorf("unrepaired catalog allowed a new decision: %q, %v", assessment.Status, err)
			}
			if report, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
				t.Errorf("unrepaired catalog allowed a new frozen report: %q, %v", report.ID, err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				if entry.section == baselineBucket {
					return putBoundJSON(tx, baselineBucket, baselineMetaBucket, original.Descriptor.Digest, original)
				}
				return tx.Bucket([]byte(entry.section)).Put([]byte(entry.key), originalIndex)
			}); err != nil {
				t.Fatal(err)
			}
			restored := f.checkReopened(t, true)
			if saved, err := restored.Baseline(secondActor, secondProject, original.Descriptor.Digest); err != nil || !reflect.DeepEqual(saved, original) {
				t.Errorf("revalidation after exact repair changed the approved record: %v", err)
			}
		})
	}
}
