package board

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupCommitRefusesMissingSavedPreviewWorkspace(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	preview := f.preview(t)
	var original previewBinding
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		var err error
		original, err = getJSON[previewBinding](tx, previewBucket, preview.Token)
		if err != nil {
			return err
		}
		changed := original
		changed.CandidateID = "C-missing"
		return putJSON(tx, previewBucket, preview.Token, changed)
	}); err != nil {
		t.Fatal(err)
	}
	receipt, err := f.app.CommitImport(secondActor, secondProject, preview.Token, f.pending)
	if !errors.Is(err, ErrIntegrity) || receipt.ID != "" {
		t.Fatalf("commit accepted an absent saved preview workspace: receipt=%q err=%v", receipt.ID, err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return putJSON(tx, previewBucket, preview.Token, original)
	}); err != nil {
		t.Fatal(err)
	}
	f.checkReopened(t, true)
}

func TestFollowupPurgeInvalidatesUncommittedImportFromTheRemovedWorkspace(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	_, _, approved, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	now := f.at.Add(181 * 24 * time.Hour)
	app, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	retained := reviewCandidate(t, app, approved, "b")
	receipt, _, prior := reviewCommit(t, app, retained, reviewRows(retained, "b", "RETAINED", Pass, now))
	upload := []byte(strings.Join(resultColumns[:], ",") + "\n" + reviewRows(retained, "b", "UNCOMMITTED", Pass, now))
	preview, err := app.PreviewImport(secondActor, secondProject, f.candidate.ID, upload)
	if err != nil || preview.Token == "" || len(preview.Issues) != 0 ||
		preview.OpenCandidateID != f.candidate.ID || !slices.Equal(preview.HistoricalCandidates, []string{retained.ID}) {
		t.Fatalf("ordinary mixed-workspace preview failed before purge: %+v %v", preview, err)
	}
	targets := []string{f.candidate.ID}
	plan, err := app.PreviewPurge(secondActor, secondProject, targets)
	if err != nil || len(plan.Ineligible) != 0 || len(plan.SharedBlockers) != 0 {
		t.Fatalf("fixture's old workspace was not eligible for scoped retention: %v", err)
	}
	if _, err := app.Purge(secondActor, secondProject, targets, plan.Confirmation); err != nil {
		t.Fatal(err)
	}
	if committed, err := app.CommitImport(secondActor, secondProject, preview.Token, upload); !errors.Is(err, ErrNotFound) || committed.ID != "" {
		t.Fatalf("purged workspace left a valid staged confirmation: receipt=%q %v", committed.ID, err)
	}
	if raw, err := app.SourceCSV(secondActor, secondProject, receipt.ID); err != nil || !bytes.Equal(raw, prior) {
		t.Fatalf("workspace purge changed independent retained source bytes: %v", err)
	}
	if current, err := app.Assess(secondActor, secondProject, retained.ID); err != nil || current.Status != Sufficient ||
		current.LedgerHighWater != receipt.LedgerHighWater || len(current.Behaviors) != 1 ||
		current.Behaviors[0].CurrentRunID != "RETAINED" || len(current.Behaviors[0].Runs) != 1 {
		t.Fatalf("purged workspace confirmation changed independent retained evidence: ledger=%d %v", current.LedgerHighWater, err)
	}
}

func TestFollowupCommitRollsBackOriginalWhenReceiptTimeCannotSerialize(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	path := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)
	app, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	f.app = app
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	raw := []byte(strings.Join(resultColumns[:], ",") + "\n")
	preview, err := app.PreviewImport(secondActor, secondProject, f.candidate.ID, raw)
	if err != nil || preview.Token == "" || len(preview.Issues) != 0 || preview.Rows != 0 {
		t.Fatalf("header-only import did not reach a valid empty staging transaction: %+v %v", preview, err)
	}
	now = now.Add(2 * time.Second)
	receipt, err := app.CommitImport(secondActor, secondProject, preview.Token, raw)
	var joined interface{ Unwrap() []error }
	if receipt.ID != "" || !errors.As(err, &joined) || len(joined.Unwrap()) != 2 {
		t.Fatalf("out-of-range receipt time did not refuse publication and report its secondary audit failure: %q %v", receipt.ID, err)
	}
	for _, cause := range joined.Unwrap() {
		var marshal *json.MarshalerError
		if !errors.As(cause, &marshal) || marshal.Type != reflect.TypeFor[*time.Time]() ||
			!strings.Contains(marshal.Error(), "year outside of range [0,9999]") {
			t.Fatalf("failure did not come from native timestamp serialization: %v", cause)
		}
	}
	t.Logf("receipt serialization failed inside the import write callback before its independently failed audit: %v", err)
	reopened := f.checkReopened(t, false)
	if data, err := reopened.SourceCSV(secondActor, secondProject, "I-"+preview.Hash); !errors.Is(err, ErrNotFound) || len(data) != 0 {
		t.Fatalf("receipt refusal left a committed original from the aborted callback: bytes=%d %v", len(data), err)
	}
	if saved, err := reopened.Receipt(secondActor, secondProject, "I-"+preview.Hash); !errors.Is(err, ErrNotFound) || saved.ID != "" {
		t.Fatalf("receipt refusal exposed a committed metadata record: %q %v", saved.ID, err)
	}
}
