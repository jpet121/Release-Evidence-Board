package board

import (
	"bytes"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

type intakeCustodyFixture struct {
	app       *Board
	candidate Candidate
	at        time.Time
	receipt   Receipt
	source    []byte
	report    Report
	html      []byte
	pending   []byte
}

func newIntakeCustodyFixture(t *testing.T) intakeCustodyFixture {
	t.Helper()
	app, baseline, at := reviewBoard(t)
	candidate := reviewCandidate(t, app, baseline, "a")
	receipt, _, source := reviewCommit(t, app, candidate, reviewRows(candidate, "a", "PRIOR", Pass, at.Add(-time.Minute)))
	report, err := app.Freeze(secondActor, secondProject, candidate.ID)
	if err != nil || report.Status != Sufficient || report.LedgerHighWater != 1 {
		t.Fatalf("fixture prior evidence is not sufficient at ledger 1: %v", err)
	}
	report, html, err := app.ReportHTML(secondActor, secondProject, report.ID)
	if err != nil || len(html) == 0 {
		t.Fatalf("fixture prior frozen report is unavailable: %v", err)
	}
	pending := []byte(strings.Join(resultColumns[:], ",") + "\n" + reviewRows(candidate, "a", "PENDING", Pass, at))
	return intakeCustodyFixture{app, candidate, at, receipt, source, report, html, pending}
}

func (f intakeCustodyFixture) preview(t *testing.T) Preview {
	t.Helper()
	p, err := f.app.PreviewImport(secondActor, secondProject, f.candidate.ID, f.pending)
	if err != nil || p.Token == "" || len(p.Issues) != 0 || len(p.Projected) != 1 ||
		p.Projected[0].Status != Sufficient || p.Projected[0].LedgerHighWater != 2 {
		t.Fatalf("fixture incoming evidence did not stage ledger 2: token=%t issues=%v err=%v", p.Token != "", p.Issues, err)
	}
	return p
}

func (f intakeCustodyFixture) checkReopened(t *testing.T, held bool) *Board {
	t.Helper()
	file := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	app, err := Open(file, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return f.at }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if receipt, err := app.Receipt(secondActor, secondProject, f.receipt.ID); err != nil || !reflect.DeepEqual(receipt, f.receipt) {
		t.Errorf("prior committed receipt changed: %v", err)
	}
	if csv, err := app.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(csv, f.source) {
		t.Errorf("prior original CSV changed: %v", err)
	}
	report, html, err := app.ReportHTML(secondActor, secondProject, f.report.ID)
	if err != nil || !reflect.DeepEqual(report, f.report) || !bytes.Equal(html, f.html) {
		t.Errorf("prior frozen decision or HTML changed: %v", err)
	}
	pendingID := "I-" + digest(f.pending)
	if receipt, err := app.Receipt(secondActor, secondProject, pendingID); !errors.Is(err, ErrNotFound) || receipt.ID != "" {
		t.Errorf("rejected intake exposed a committed receipt %q: %v", receipt.ID, err)
	}
	if raw, err := app.SourceCSV(secondActor, secondProject, pendingID); !errors.Is(err, ErrNotFound) || len(raw) != 0 {
		t.Errorf("rejected intake exposed a committed original: %d bytes, %v", len(raw), err)
	}
	if suspended, err := app.AssessmentSuspended(secondActor, secondProject); err != nil || suspended != held {
		t.Errorf("reopened hold=%t, want %t: %v", suspended, held, err)
	}
	if held {
		if err := app.Revalidate(secondActor, secondProject); err != nil {
			t.Fatalf("revalidate after removing the isolated fault: %v", err)
		}
	}
	a, err := app.Assess(secondActor, secondProject, f.candidate.ID)
	if err != nil || a.Status != Sufficient || a.LedgerHighWater != 1 || len(a.Behaviors) != 1 ||
		a.Behaviors[0].CurrentRunID != "PRIOR" || len(a.Behaviors[0].Runs) != 1 {
		t.Errorf("rejected intake changed the prior assessment, run inventory or ledger: ledger=%d, %v", a.LedgerHighWater, err)
	}
	return app
}

func TestFollowupMalformedRetainedIdentityCannotSupportNewDecisions(t *testing.T) {
	for _, fault := range []string{"material_index", "material_identity", "manifest_descriptor", "candidate_record_id", "run_index_key"} {
		t.Run(fault, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			candidate, manifest, baseline, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			other := reviewCandidate(t, f.app, baseline, "b")
			staged := f.preview(t)
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				changed := candidate
				switch fault {
				case "material_index":
					return putJSON(tx, materialBucket, candidate.MaterialDigest, other.ID)
				case "material_identity":
					changed.MaterialDigest = other.MaterialDigest
				case "manifest_descriptor":
					manifestCopy := manifest
					manifestCopy.Descriptor.Text += "\nchanged stored descriptor text"
					return putJSON(tx, manifestBucket, candidate.ManifestDigest, manifestCopy)
				case "candidate_record_id":
					changed.ID = other.ID
				case "run_index_key":
					index := tx.Bucket([]byte(candidateRun))
					if err := index.Delete([]byte(candidate.ID + "\x00PRIOR")); err != nil {
						return err
					}
					return index.Put([]byte(candidate.ID+"\x00WRONG-INDEX"), []byte("PRIOR"))
				}
				// The record is self-consistent at the byte binding. Only its
				// relationship to the independently verified catalog differs.
				return putBoundJSON(tx, candidateBucket, candidateMetaBucket, candidate.ID, changed)
			}); err != nil {
				t.Fatal(err)
			}

			a, err := f.app.Assess(secondActor, secondProject, candidate.ID)
			if !errors.Is(err, ErrIntegrity) || a.Status != "" {
				t.Fatalf("invalid retained evidence produced an assessment %q: %v", a.Status, err)
			}
			if report, err := f.app.Freeze(secondActor, secondProject, candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
				t.Errorf("known integrity fault allowed a new frozen export %q: %v", report.ID, err)
			}
			if preview, err := f.app.PreviewImport(secondActor, secondProject, candidate.ID, f.pending); !errors.Is(err, ErrIntegrity) || preview.Token != "" {
				t.Errorf("known integrity fault allowed a new preview: token=%t, %v", preview.Token != "", err)
			}
			if receipt, err := f.app.CommitImport(secondActor, secondProject, staged.Token, f.pending); !errors.Is(err, ErrIntegrity) || receipt.ID != "" {
				t.Errorf("known integrity fault allowed confirmation of earlier staging %q: %v", receipt.ID, err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				switch fault {
				case "material_index":
					return putJSON(tx, materialBucket, candidate.MaterialDigest, candidate.ID)
				case "manifest_descriptor":
					return putJSON(tx, manifestBucket, candidate.ManifestDigest, manifest)
				case "run_index_key":
					index := tx.Bucket([]byte(candidateRun))
					if err := index.Delete([]byte(candidate.ID + "\x00WRONG-INDEX")); err != nil {
						return err
					}
					return index.Put([]byte(candidate.ID+"\x00PRIOR"), []byte("PRIOR"))
				}
				return putBoundJSON(tx, candidateBucket, candidateMetaBucket, candidate.ID, candidate)
			}); err != nil {
				t.Fatal(err)
			}
			f.checkReopened(t, true)
		})
	}
}

func TestFollowupPreviewRejectsOriginalWithoutReceipt(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	staged := f.preview(t)
	key := []byte(staged.Hash)
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(csvBucket)).Put(key, f.pending)
	}); err != nil {
		t.Fatal(err)
	}
	preview, err := f.app.PreviewImport(secondActor, secondProject, f.candidate.ID, f.pending)
	if preview.Token != "" || len(preview.Projected) != 0 || !errors.Is(err, ErrIntegrity) {
		t.Fatalf("orphan original unexpectedly produced a usable preview: token=%t decisions=%d %v", preview.Token != "", len(preview.Projected), err)
	}
	if receipt, err := f.app.CommitImport(secondActor, secondProject, staged.Token, f.pending); !errors.Is(err, ErrIntegrity) || receipt.ID != "" {
		t.Errorf("confirming orphaned evidence returned receipt %q: %v", receipt.ID, err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(csvBucket)).Delete(key)
	}); err != nil {
		t.Fatal(err)
	}
	f.checkReopened(t, true)
}

func TestFollowupPreviewRechecksContributorBeforeStaging(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	const contributor, password = "intake-contributor", "synthetic-contributor-password"
	if err := f.app.SetAccount(secondActor, contributor, password, []Role{Contributor}); err != nil {
		t.Fatal(err)
	}
	previous, err := f.app.PreviewImport(contributor, secondProject, f.candidate.ID, f.pending)
	if err != nil || previous.Token == "" || len(previous.Issues) != 0 {
		t.Fatalf("contributor could not stage the pending fixture: %v", err)
	}

	writer, err := f.app.db.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if writer != nil {
			if err := writer.Rollback(); err != nil {
				t.Error(err)
			}
		}
	}()
	type outcome struct {
		preview Preview
		err     error
	}
	result := make(chan outcome, 1)
	before := f.app.db.Stats().TxN
	go func() {
		p, err := f.app.PreviewImport(contributor, secondProject, f.candidate.ID, f.pending)
		result <- outcome{p, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.app.db.Stats().TxN == before {
		if time.Now().After(deadline) {
			t.Fatal("preview did not enter its original read snapshot")
		}
		runtime.Gosched()
	}
	// Change one live role record in the held real writer transaction. The
	// preview reader can see the earlier role, but staging sees this commit.
	record, err := getJSON[accountRecord](writer, accountBucket, contributor)
	if err != nil {
		t.Fatal(err)
	}
	record.Roles = []Role{Viewer}
	if err := putJSON(writer, accountBucket, contributor, record); err != nil {
		t.Fatal(err)
	}
	err = writer.Commit()
	writer = nil
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if !errors.Is(got.err, ErrForbidden) || got.preview.Token != "" || len(got.preview.Projected) != 0 {
			t.Fatalf("revoked contributor received a staged preview: token=%t decisions=%d %v", got.preview.Token != "", len(got.preview.Projected), got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("preview did not finish after the authority transaction")
	}
	if receipt, err := f.app.CommitImport(contributor, secondProject, previous.Token, f.pending); !errors.Is(err, ErrForbidden) || receipt.ID != "" {
		t.Errorf("revoked contributor used its earlier confirmation %q: %v", receipt.ID, err)
	}
	f.checkReopened(t, false)
}

func TestFollowupCommitRollsBackSeparateIntakeWrites(t *testing.T) {
	for _, kind := range []string{"run_record", "run_index", "result_index", "original_csv", "receipt"} {
		t.Run(kind, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			p := f.preview(t)
			bucket, key := runBucket, "PENDING"
			switch kind {
			case "run_index":
				bucket, key = candidateRun, f.candidate.ID+"\x00PENDING"
			case "result_index":
				bucket, key = resultBucket, "RESPENDING-2"
			case "original_csv":
				bucket, key = csvBucket, p.Hash
			case "receipt":
				bucket, key = receiptBucket, p.Hash
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				_, err := tx.Bucket([]byte(bucket)).CreateBucket([]byte(key))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			// Receipt metadata now supplies a ledger watermark. The planner
			// detects an invalid receipt slot before reaching its write.
			wantsIntegrityHold := kind == "receipt"
			wantErr := bolt.ErrIncompatibleValue
			if wantsIntegrityHold {
				wantErr = ErrIntegrity
			}
			if receipt, err := f.app.CommitImport(secondActor, secondProject, p.Token, f.pending); !errors.Is(err, wantErr) || receipt.ID != "" {
				t.Errorf("invalid %s storage returned receipt %q: %v", kind, receipt.ID, err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(bucket)).DeleteBucket([]byte(key))
			}); err != nil {
				t.Fatal(err)
			}
			f.checkReopened(t, wantsIntegrityHold)
		})
	}
}
