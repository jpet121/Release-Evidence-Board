package board

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupCandidateReadsRejectIncompleteCatalogPairs(t *testing.T) {
	cases := []struct{ name, bucket, expected string }{
		{"missing candidate", candidateBucket, "retained candidate record is missing"},
		{"missing approval", baselineBucket, "retained approved baseline record is missing"},
		{"malformed candidate", candidateBucket, "invalid storage record in " + candidateBucket},
		{"malformed approval", baselineBucket, "invalid storage record in " + baselineBucket},
		{"different catalog key", candidateBucket, "candidate key is inconsistent"},
	}
	for _, entry := range cases {
		t.Run(entry.name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			candidate := f.candidate
			key := candidate.ID
			if entry.bucket == baselineBucket {
				key = candidate.BaselineDigest
			}
			other := "C-" + strings.Repeat("f", 64)
			var original []byte
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				bucket := tx.Bucket([]byte(entry.bucket))
				original = bytes.Clone(bucket.Get([]byte(key)))
				if original == nil || bucket.Get([]byte(other)) != nil {
					return errors.New("candidate catalog fixture has unexpected records")
				}
				if entry.name == "different catalog key" {
					return putBoundJSON(tx, candidateBucket, candidateMetaBucket, other, candidate)
				}
				if strings.HasPrefix(entry.name, "malformed ") {
					return bucket.Put([]byte(key), []byte("{"))
				}
				return bucket.Delete([]byte(key))
			}); err != nil {
				t.Fatal(err)
			}

			var err error
			if entry.name == "different catalog key" {
				_, err = f.app.Candidates(secondActor, secondProject)
			} else {
				_, _, _, err = f.app.Candidate(secondActor, secondProject, candidate.ID)
			}
			if !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), entry.expected) {
				t.Errorf("candidate read did not report incomplete catalog custody %q: %v", entry.expected, err)
			}
			if entry.name == "missing candidate" {
				if _, err := f.app.Candidates(secondActor, secondProject); !errors.Is(err, ErrIntegrity) ||
					!strings.Contains(err.Error(), "stored evidence companion lacks a primary record") {
					t.Errorf("candidate catalog accepted a companion without its record: %v", err)
				}
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				if entry.name != "different catalog key" {
					return tx.Bucket([]byte(entry.bucket)).Put([]byte(key), original)
				}
				if err := tx.Bucket([]byte(candidateBucket)).Delete([]byte(other)); err != nil {
					return err
				}
				return tx.Bucket([]byte(candidateMetaBucket)).Delete([]byte(other))
			}); err != nil {
				t.Fatal(err)
			}
			f.checkReopened(t, true)
		})
	}
}

func TestFollowupCandidateRequiresItsRetainedApprovalAndExplicitPredecessor(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	_, previous, approval, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	input := CandidateInput{
		Manifest: previous.Manifest, BaselineDigest: approval.Descriptor.Digest,
		PredecessorID: "C-" + strings.Repeat("f", 64),
	}
	input.Manifest.BuildDigest = strings.Repeat("b", 64)
	if result, reused, err := f.app.CreateCandidate(secondActor, secondProject, input); !errors.Is(err, ErrNotFound) || result.ID != "" || reused {
		t.Fatalf("new candidate with an absent explicit predecessor returned %q, reused=%t, %v", result.ID, reused, err)
	}
	if catalog, err := f.app.Candidates(secondActor, secondProject); err != nil || len(catalog) != 1 || catalog[0].ID != f.candidate.ID {
		t.Fatalf("missing predecessor changed the candidate catalog: %v", err)
	}
	input.PredecessorID = f.candidate.ID
	if result, reused, err := f.app.CreateCandidate(secondActor, secondProject, input); err != nil || result.PredecessorID != f.candidate.ID || reused {
		t.Fatalf("new candidate with its exact retained predecessor returned %q, reused=%t, %v", result.PredecessorID, reused, err)
	}

	key := []byte(f.candidate.BaselineDigest)
	var saved [][]byte
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		for _, section := range []string{baselineBucket, baselineMetaBucket} {
			bucket := tx.Bucket([]byte(section))
			value := bytes.Clone(bucket.Get(key))
			if len(value) == 0 {
				return errors.New("fixture does not hold the complete approved pair")
			}
			saved = append(saved, value)
			if err := bucket.Delete(key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if selected, _, _, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) ||
		!strings.Contains(err.Error(), "retained candidate baseline is missing") || selected.ID != "" {
		t.Fatalf("known candidate with a wholly absent approval did not report custody loss: %q, %v", selected.ID, err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		for i, section := range []string{baselineBucket, baselineMetaBucket} {
			if err := tx.Bucket([]byte(section)).Put(key, saved[i]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reopened := f.checkReopened(t, true)
	if err := reopened.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("repaired approval pair did not revalidate: %v", err)
	}
}

func TestFollowupAssessmentRefusesInconsistentRetainedRunReference(t *testing.T) {
	for _, name := range []string{"missing_run", "wrong_candidate", "wrong_run_id", "wrong_index_suffix"} {
		t.Run(name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			_, _, approved, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			other := reviewCandidate(t, f.app, approved, "b")
			if len(f.receipt.AddedRuns) != 1 {
				t.Fatal("fixture requires one committed run")
			}
			runID := f.receipt.AddedRuns[0]
			exactKey := []byte(f.candidate.ID + "\x00" + runID)
			wrongKey := []byte(f.candidate.ID + "\x00different-local-run")
			var original []byte
			err = f.app.db.Update(func(tx *bolt.Tx) error {
				runs := tx.Bucket([]byte(runBucket))
				original = bytes.Clone(runs.Get([]byte(runID)))
				run, err := getJSON[Run](tx, runBucket, runID)
				if err != nil {
					return err
				}
				switch name {
				case "missing_run":
					return runs.Delete([]byte(runID))
				case "wrong_candidate":
					run.CandidateID = other.ID
				case "wrong_run_id":
					run.ID = "different-local-run"
				case "wrong_index_suffix":
					index := tx.Bucket([]byte(candidateRun))
					if value := index.Get(exactKey); string(value) != runID {
						return errors.New("fixture lacks its own committed index mapping")
					}
					if err := index.Delete(exactKey); err != nil {
						return err
					}
					return index.Put(wrongKey, []byte(runID))
				}
				return putJSON(tx, runBucket, runID, run)
			})
			if err != nil {
				t.Fatal(err)
			}
			if a, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) ||
				!strings.Contains(err.Error(), "run record missing or inconsistent") || a.Status != "" {
				t.Fatalf("assessment did not refuse the exact inconsistent run reference: status=%q %v", a.Status, err)
			}
			if report, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
				t.Fatalf("held inconsistent run reference exposed a new report: %q %v", report.ID, err)
			}
			err = f.app.db.Update(func(tx *bolt.Tx) error {
				if name == "wrong_index_suffix" {
					index := tx.Bucket([]byte(candidateRun))
					if err := index.Delete(wrongKey); err != nil {
						return err
					}
					return index.Put(exactKey, []byte(runID))
				}
				return tx.Bucket([]byte(runBucket)).Put([]byte(runID), original)
			})
			if err != nil {
				t.Fatal(err)
			}
			f.checkReopened(t, true)
		})
	}
}
