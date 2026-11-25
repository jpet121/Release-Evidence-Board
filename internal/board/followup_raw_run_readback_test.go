package board

import (
	"bytes"
	"encoding/csv"
	"errors"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupStoredReceiptReconstructsOnlyUsableOriginalRows(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{"unparseable_original", "receipt row count or parser version differs from original CSV"},
		{"conflicting_run_rows", "original CSV contains conflicting run metadata"},
		{"absent_evidence_candidate", "receipt lacks a retained evidence candidate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			rows, issues := ParseResultsCSV(f.pending)
			if len(issues) != 0 || len(rows) != 2 {
				t.Fatalf("fixture does not contain the two expected source rows: %v", issues)
			}
			receipt := f.receipt
			receipt.AddedRuns = []string{rows[0].RunID()}
			switch tc.name {
			case "conflicting_run_rows":
				rows[1].Fields[colRunFinished] = FormatUTC(f.at.Add(time.Second))
			case "absent_evidence_candidate":
				newManifest := strings.Repeat("e", 64)
				missing := "C-" + digest([]byte(newManifest+rows[0].Fields[colBaseline]))
				for i := range rows {
					rows[i].Fields[colManifest] = newManifest
				}
				receipt.CandidateIDs = []string{missing}
				receipt.HistoricalCandidates = []string{missing}
			}
			var body bytes.Buffer
			writer := csv.NewWriter(&body)
			if err := writer.Write(resultColumns[:]); err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if err := writer.Write(row.Fields[:]); err != nil {
					t.Fatal(err)
				}
			}
			writer.Flush()
			if err := writer.Error(); err != nil {
				t.Fatal(err)
			}
			raw := body.Bytes()
			if tc.name == "unparseable_original" {
				raw = []byte("invalid,header\n")
			} else if parsed, diagnostics := ParseResultsCSV(raw); len(parsed) != 2 || len(diagnostics) != 0 {
				t.Fatalf("row-link fixture does not individually parse: %v", diagnostics)
			}
			receipt.Hash = digest(raw)
			receipt.ID = "I-" + receipt.Hash
			if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				if err := tx.Bucket([]byte(csvBucket)).Put([]byte(receipt.Hash), raw); err != nil {
					return err
				}
				return putJSON(tx, receiptBucket, receipt.Hash, receipt)
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("whole-store revalidation did not identify the isolated source reconstruction defect: %v", err)
			}
			if report, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
				t.Fatalf("invalid source reconstruction exposed a new frozen decision: %q %v", report.ID, err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				if err := tx.Bucket([]byte(receiptBucket)).Delete([]byte(receipt.Hash)); err != nil {
					return err
				}
				return tx.Bucket([]byte(csvBucket)).Delete([]byte(receipt.Hash))
			}); err != nil {
				t.Fatal(err)
			}
			f.checkReopened(t, true)
		})
	}
}

func TestFollowupWholeStoreRevalidationChecksRunAndResultIndexesAfterRawReadback(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{"extra_run_record", "run metadata or complete source row set differs from original CSV"},
		{"stored_run_id_mismatch", "run metadata or complete source row set differs from original CSV"},
		{"stored_run_candidate_mismatch", "run metadata or complete source row set differs from original CSV"},
		{"malformed_result_link", "result index is inconsistent"},
		{"extra_candidate_run_key", "candidate run index is inconsistent"},
		{"wrong_candidate_run_target", "candidate run index is inconsistent"},
		{"missing_run_for_exact_replay", "retained duplicate rows differ from original CSV"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			runID := f.receipt.AddedRuns[0]
			var otherCandidateID string
			if tc.name == "stored_run_candidate_mismatch" {
				_, _, approved, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
				if err != nil {
					t.Fatal(err)
				}
				otherCandidateID = reviewCandidate(t, f.app, approved, "b").ID
			}
			if tc.name == "missing_run_for_exact_replay" {
				_, rows, _ := strings.Cut(string(f.source), "\n")
				body := strings.Replace(rows, ",PASS,,", ",\"PASS\",,", 1)
				duplicate, _, _ := reviewCommit(t, f.app, f.candidate, body)
				if duplicate.ID == f.receipt.ID || len(duplicate.ExistingRuns) != 1 || duplicate.ExistingRuns[0] != runID {
					t.Fatalf("fixture did not replay the actual retained run independently: %+v", duplicate)
				}
			}
			if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			var section, changedKey string
			var original []byte
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				run, err := getJSON[Run](tx, runBucket, runID)
				if err != nil {
					return err
				}
				section, changedKey = runBucket, runID
				switch tc.name {
				case "extra_run_record":
					changedKey = "EXTRA-RAWLESS-RUN"
					run.ID = changedKey
				case "stored_run_id_mismatch":
					run.ID = "MISNAMED-RAW-RUN"
				case "stored_run_candidate_mismatch":
					run.CandidateID = otherCandidateID
				case "malformed_result_link":
					section, changedKey = resultBucket, run.Rows[0].ResultID()
				case "extra_candidate_run_key":
					section, changedKey = candidateRun, f.candidate.ID+"\x00EXTRA-INDEX-KEY"
				case "wrong_candidate_run_target":
					section, changedKey = candidateRun, f.candidate.ID+"\x00"+runID
				}
				bucket := tx.Bucket([]byte(section))
				key := []byte(changedKey)
				original = bytes.Clone(bucket.Get(key))
				switch tc.name {
				case "missing_run_for_exact_replay":
					return bucket.Delete(key)
				case "malformed_result_link":
					return bucket.Put(key, []byte("{"))
				case "extra_candidate_run_key", "wrong_candidate_run_target":
					return bucket.Put(key, []byte("DIFFERENT-RAW-RUN"))
				default:
					return putJSON(tx, section, changedKey, run)
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("revalidation did not report the exact independent run or result linkage fault: %v", err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				bucket := tx.Bucket([]byte(section))
				key := []byte(changedKey)
				if original == nil {
					return bucket.Delete(key)
				}
				return bucket.Put(key, original)
			}); err != nil {
				t.Fatal(err)
			}
			f.checkReopened(t, true)
		})
	}
}
