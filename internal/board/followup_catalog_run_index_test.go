package board

import (
	"errors"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupRevalidationRejectsIndependentCatalogAndRunIndexFaults(t *testing.T) {
	cases := []struct{ name, reason string }{
		{"retained tombstone", "retained candidate also has a purge marker"},
		{"self predecessor", "candidate predecessor reference is inconsistent"},
		{"missing predecessor", "candidate predecessor reference is inconsistent"},
		{"orphan manifest", "manifest has no matching candidate and digest key"},
		{"orphan manifest at wrong key", "manifest has no matching candidate and digest key"},
		{"malformed result index", "result index entry is invalid"},
		{"orphan result index", "result index refers to a missing source row"},
	}
	for _, entry := range cases {
		t.Run(entry.name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			candidate, oldManifest, _, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			var bucket, key string
			err = f.app.db.Update(func(tx *bolt.Tx) error {
				switch entry.name {
				case "retained tombstone":
					bucket, key = tombstoneBucket, candidate.ID
					return tx.Bucket([]byte(bucket)).Put([]byte(key), []byte(FormatUTC(f.at)))
				case "self predecessor", "missing predecessor":
					proposed := candidate
					proposed.PredecessorID = candidate.ID
					if entry.name == "missing predecessor" {
						proposed.PredecessorID = "C-" + strings.Repeat("f", 64)
					}
					return putBoundJSON(tx, candidateBucket, candidateMetaBucket, candidate.ID, proposed)
				case "orphan manifest", "orphan manifest at wrong key":
					manifest := oldManifest.Manifest
					manifest.BuildDigest = strings.Repeat("b", 64)
					descriptor, err := manifest.Descriptor()
					if err != nil {
						return err
					}
					bucket, key = manifestBucket, descriptor.Digest
					if entry.name == "orphan manifest at wrong key" {
						key = strings.Repeat("f", 64)
					}
					return putJSON(tx, bucket, key, StoredManifest{Manifest: manifest, Descriptor: descriptor})
				case "malformed result index":
					bucket, key = resultBucket, "RESULT-WITHOUT-SOURCE"
					return tx.Bucket([]byte(bucket)).Put([]byte(key), []byte("{"))
				case "orphan result index":
					bucket, key = resultBucket, "RESULT-WITHOUT-SOURCE"
					return putJSON(tx, bucket, key, "RUN-WITHOUT-SOURCE")
				}
				return errors.New("undefined custody fixture")
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), entry.reason) {
				t.Errorf("reviewer did not reject the isolated index fault %q: %v", entry.reason, err)
			}
			if a, err := f.app.Assess(secondActor, secondProject, candidate.ID); !errors.Is(err, ErrIntegrity) || a.Status != "" {
				t.Errorf("the stored index fault allowed a new assessment: %q, %v", a.Status, err)
			}
			if report, err := f.app.Freeze(secondActor, secondProject, candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
				t.Errorf("the stored index fault allowed a new report: %q, %v", report.ID, err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				if bucket == "" {
					return putBoundJSON(tx, candidateBucket, candidateMetaBucket, candidate.ID, candidate)
				}
				return tx.Bucket([]byte(bucket)).Delete([]byte(key))
			}); err != nil {
				t.Fatal(err)
			}
			f.checkReopened(t, true)
		})
	}
}
