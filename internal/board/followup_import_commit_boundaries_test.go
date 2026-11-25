package board

import (
	"bytes"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupCommitRejectsAbsentOrUnusableStagedOriginals(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"missing_original", ErrNotFound},
		{"oversized_input", ErrConflict},
		{"malformed_original", ErrInvalid},
		{"result_identity_now_in_use", ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			preview := f.preview(t)
			key := []byte(preview.Token)
			var request []byte
			var resultID string
			newHash := preview.Hash
			err := f.app.db.Update(func(tx *bolt.Tx) error {
				originals := tx.Bucket([]byte(previewCSV))
				switch tc.name {
				case "missing_original":
					return originals.Delete(key)
				case "oversized_input":
					request = bytes.Repeat([]byte("x"), MaxCSVBytes+1)
				case "malformed_original":
					// Give the reparse guard a stored original whose byte
					// binding still matches but whose grammar does not.
					changed := []byte("invalid,header\n")
					newHash = digest(changed)
					binding, err := getJSON[previewBinding](tx, previewBucket, preview.Token)
					if err != nil {
						return err
					}
					binding.Hash = newHash
					if err := putJSON(tx, previewBucket, preview.Token, binding); err != nil {
						return err
					}
					return originals.Put(key, changed)
				case "result_identity_now_in_use":
					rows, issues := ParseResultsCSV(f.pending)
					if len(rows) == 0 || len(issues) != 0 {
						return errors.New("fixture does not contain a known-valid pending result")
					}
					resultID = rows[0].ResultID()
					return putJSON(tx, resultBucket, resultID, f.receipt.AddedRuns[0])
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := f.app.CommitImport(secondActor, secondProject, preview.Token, request)
			if result.ID != "" || !errors.Is(err, tc.want) ||
				(tc.want == ErrInvalid && !strings.Contains(err.Error(), "CSV no longer passes validation")) {
				t.Fatalf("commit did not reject the unusable staged original at the expected guard: receipt=%q err=%v", result.ID, err)
			}
			if receipt, err := f.app.Receipt(secondActor, secondProject, "I-"+newHash); receipt.ID != "" || !errors.Is(err, ErrNotFound) {
				t.Fatalf("refused commit exposed a receipt for the rejected bytes: %q %v", receipt.ID, err)
			}
			if resultID != "" {
				if err := f.app.db.Update(func(tx *bolt.Tx) error {
					return tx.Bucket([]byte(resultBucket)).Delete([]byte(resultID))
				}); err != nil {
					t.Fatal(err)
				}
			}
			f.checkReopened(t, false)
		})
	}
}

func TestFollowupRejectedPreviewPreservesTerminalCounterAndEvidence(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(metaBucket)).Put([]byte("import_rejections"), strconv.AppendUint(nil, math.MaxUint64, 10))
	}); err != nil {
		t.Fatal(err)
	}
	preview, err := f.app.PreviewImport(secondActor, secondProject, f.candidate.ID, []byte("invalid,header\n"))
	if preview.Token != "" || len(preview.Issues) == 0 || !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "storage counter capacity is exhausted") {
		t.Fatalf("rejected preview hid a real refusal to advance its exhausted counter: token=%q issues=%d %v", preview.Token, len(preview.Issues), err)
	}
	health, err := f.app.Health(secondActor, secondProject)
	if err != nil || health.ImportRejections != math.MaxUint64 {
		t.Fatalf("refused rejection record altered its terminal counter: %d %v", health.ImportRejections, err)
	}
	events, err := f.app.Audit(secondActor, secondProject)
	if err != nil || slices.ContainsFunc(events, func(event AuditEvent) bool { return event.Action == "import.reject" }) {
		t.Fatalf("failed rejection-record transaction exposed an audit success or partial failure event: %v", err)
	}
	f.checkReopened(t, false)
}

func TestFollowupOneUploadCannotAssignSameRunToTwoCandidates(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	_, _, approval, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	other := reviewCandidate(t, f.app, approval, "b")
	const run = "SHARED-LOCAL-RUN"
	body := strings.Join(resultColumns[:], ",") + "\n" +
		reviewRows(f.candidate, "a", run, Pass, f.at) +
		reviewRows(other, "b", run, Pass, f.at)
	preview, err := f.app.PreviewImport(secondActor, secondProject, f.candidate.ID, []byte(body))
	if err != nil || preview.Token != "" || len(preview.Issues) == 0 ||
		slices.ContainsFunc(preview.Issues, func(issue Diagnostic) bool { return issue.Code != "RUN_IDENTITY" }) {
		t.Fatalf("mixed-candidate run did not return the exact run identity diagnostic: token=%q %+v %v", preview.Token, preview.Issues, err)
	}
	if saved, err := f.app.Receipt(secondActor, secondProject, "I-"+digest([]byte(body))); saved.ID != "" || !errors.Is(err, ErrNotFound) {
		t.Fatalf("refused mixed-candidate upload exposed a receipt: %q %v", saved.ID, err)
	}
	f.checkReopened(t, false)
}
