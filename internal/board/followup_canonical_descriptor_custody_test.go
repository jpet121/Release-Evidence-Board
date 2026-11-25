package board

import (
	"errors"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupCandidateChecksEachStoredCanonicalDescriptorBoundary(t *testing.T) {
	for _, fault := range []string{"invalid_manifest", "valid_manifest_at_wrong_key", "noncanonical_id", "different_product"} {
		t.Run(fault, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			candidate, manifest, _, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			changedManifest := manifest
			changedCandidate := candidate
			manifestKey := candidate.ManifestDigest
			switch fault {
			case "invalid_manifest":
				changedManifest.Manifest.BuildDigest = ""
			case "valid_manifest_at_wrong_key":
				changedManifest.Manifest.BuildDigest = strings.Repeat("c", 64)
			case "noncanonical_id":
				changedCandidate.ID = "C-" + strings.Repeat("e", 64)
			case "different_product":
				changedManifest.Manifest.ProductID = "OTHER"
			}
			if fault == "valid_manifest_at_wrong_key" || fault == "different_product" {
				changedManifest.Descriptor, err = changedManifest.Manifest.Descriptor()
				if err != nil {
					t.Fatal(err)
				}
			}
			if fault == "different_product" {
				manifestKey = changedManifest.Descriptor.Digest
				changedCandidate.ManifestDigest = manifestKey
				changedCandidate.ID = "C-" + digest([]byte(manifestKey+changedCandidate.BaselineDigest))
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				if err := putJSON(tx, manifestBucket, manifestKey, changedManifest); err != nil {
					return err
				}
				if changedCandidate.ID == candidate.ID {
					return nil
				}
				return putBoundJSON(tx, candidateBucket, candidateMetaBucket, changedCandidate.ID, changedCandidate)
			}); err != nil {
				t.Fatal(err)
			}
			if record, _, _, err := f.app.Candidate(secondActor, secondProject, changedCandidate.ID); record.ID != "" ||
				!errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "candidate descriptors or identity differ from approved bytes") {
				t.Fatalf("candidate lookup accepted an invalid stored canonical descriptor or identity: %q %v", record.ID, err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				if changedCandidate.ID != candidate.ID {
					key := []byte(changedCandidate.ID)
					if err := tx.Bucket([]byte(candidateBucket)).Delete(key); err != nil {
						return err
					}
					if err := tx.Bucket([]byte(candidateMetaBucket)).Delete(key); err != nil {
						return err
					}
				}
				if manifestKey != candidate.ManifestDigest {
					return tx.Bucket([]byte(manifestBucket)).Delete([]byte(manifestKey))
				}
				return putJSON(tx, manifestBucket, candidate.ManifestDigest, manifest)
			}); err != nil {
				t.Fatal(err)
			}
			f.checkReopened(t, true)
		})
	}
}

func TestFollowupApprovedBaselineRequiresItsOwnDigestAndProjectProduct(t *testing.T) {
	for _, fault := range []string{"canonical_roster_at_wrong_key", "canonical_foreign_roster"} {
		t.Run(fault, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			_, _, original, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			key := strings.Repeat("f", 64)
			changed := original
			if fault == "canonical_foreign_roster" {
				changed.Baseline.ProductID = "OTHER"
				changed.Descriptor, err = changed.Baseline.Descriptor()
				if err != nil {
					t.Fatal(err)
				}
				key = changed.Descriptor.Digest
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				return putBoundJSON(tx, baselineBucket, baselineMetaBucket, key, changed)
			}); err != nil {
				t.Fatal(err)
			}
			if approval, err := f.app.Baseline(secondActor, secondProject, key); approval.Descriptor.Digest != "" ||
				!errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "approved baseline differs from canonical bytes") {
				t.Fatalf("approved baseline lookup accepted a canonical roster at the wrong identity: %q %v", approval.Descriptor.Digest, err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				if err := tx.Bucket([]byte(baselineBucket)).Delete([]byte(key)); err != nil {
					return err
				}
				return tx.Bucket([]byte(baselineMetaBucket)).Delete([]byte(key))
			}); err != nil {
				t.Fatal(err)
			}
			f.checkReopened(t, true)
		})
	}
}
