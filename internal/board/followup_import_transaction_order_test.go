package board

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Hold the real writer until the pinned bbolt SDK reports that the only
// concurrent call has opened its old read snapshot. The caller's next writer
// must then observe the committed update. No sleep chooses the ordering.
func afterOldStoreRead[T any](t *testing.T, app *Board, writer func(func()) error, operation func() T) T {
	t.Helper()
	writerEntered, permitCommit := make(chan struct{}), make(chan struct{})
	writerDone, observed := make(chan error, 1), make(chan T, 1)
	var jobs sync.WaitGroup
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(permitCommit) }) }
	defer func() {
		release()
		jobs.Wait()
	}()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	jobs.Go(func() {
		writerDone <- writer(func() {
			close(writerEntered)
			<-permitCommit
		})
	})
	select {
	case <-writerEntered:
	case err := <-writerDone:
		t.Fatalf("writer did not hold its pending transaction: %v", err)
	case <-deadline.C:
		t.Fatal("writer did not reach its bounded transaction pause")
	}
	before := app.db.Stats().TxN
	if before == 0 {
		t.Fatal("pinned store does not report the already completed fixture reads")
	}
	jobs.Go(func() { observed <- operation() })
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for app.db.Stats().TxN == before {
		select {
		case <-tick.C:
		case <-observed:
			t.Fatal("public operation returned without observing the old snapshot")
		case <-deadline.C:
			t.Fatal("public operation did not acquire a native read snapshot")
		}
	}
	if got := app.db.Stats().TxN; got != before+1 {
		t.Fatalf("unrelated reads changed the snapshot premise: before=%d after=%d", before, got)
	}
	select {
	case <-observed:
		t.Fatal("public operation finished before the new transaction could commit")
	default:
	}
	release()
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("staged transaction could not commit: %v", err)
		}
	case <-deadline.C:
		t.Fatal("the staged native writer did not join")
	}
	select {
	case result := <-observed:
		return result
	case <-deadline.C:
		t.Fatal("public operation did not join after writer commit")
		var zero T
		return zero
	}
}

func TestFollowupCommitChecksInputsAgainAfterItsOldRead(t *testing.T) {
	for _, tc := range []struct {
		name, section, detail string
		want                  error
	}{
		{"staging_cancelled", previewBucket, "", ErrNotFound},
		{"receipt_no_longer_decodes", receiptBucket, "invalid storage record in receipts", ErrIntegrity},
		{"revision_no_longer_decodes", metaBucket, "invalid storage counter", ErrIntegrity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			staged := f.preview(t)
			key := []byte(staged.Token)
			if tc.section == receiptBucket {
				key = []byte(staged.Hash)
			} else if tc.section == metaBucket {
				key = []byte("data_revision")
			}
			var prior []byte
			write := func(pause func()) error {
				return f.app.db.Update(func(tx *bolt.Tx) error {
					bucket := tx.Bucket([]byte(tc.section))
					prior = bytes.Clone(bucket.Get(key))
					var err error
					if tc.section == previewBucket {
						err = bucket.Delete(key)
						if err == nil {
							err = tx.Bucket([]byte(previewCSV)).Delete(key)
						}
					} else {
						err = bucket.Put(key, []byte("{"))
					}
					if err != nil {
						return err
					}
					pause()
					return nil
				})
			}
			type result struct {
				receipt Receipt
				err     error
			}
			commit := afterOldStoreRead(t, f.app, write, func() result {
				receipt, err := f.app.CommitImport(secondActor, secondProject, staged.Token, nil)
				return result{receipt, err}
			})
			if commit.receipt.ID != "" || !errors.Is(commit.err, tc.want) ||
				(tc.detail != "" && !strings.Contains(commit.err.Error(), tc.detail)) {
				t.Fatalf("import did not report the exact state change after its old read: receipt=%q %v", commit.receipt.ID, commit.err)
			}
			if tc.section != previewBucket {
				if err := f.app.db.Update(func(tx *bolt.Tx) error {
					bucket := tx.Bucket([]byte(tc.section))
					if prior == nil {
						return bucket.Delete(key)
					}
					return bucket.Put(key, prior)
				}); err != nil {
					t.Fatal(err)
				}
			}
			f.checkReopened(t, tc.want == ErrIntegrity)
		})
	}
}

func TestFollowupPreviewDoesNotStageAgainstAChangedRevision(t *testing.T) {
	for _, fault := range []string{"real_new_candidate", "malformed_saved_revision"} {
		t.Run(fault, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			candidate, manifest, _, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			path := f.app.db.Path()
			if err := f.app.Close(); err != nil {
				t.Fatal(err)
			}
			var armed atomic.Bool
			var pauseWriter func()
			app, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time {
				if armed.CompareAndSwap(true, false) {
					pauseWriter()
				}
				return f.at
			}})
			if err != nil {
				t.Fatal(err)
			}
			f.app = app
			t.Cleanup(func() {
				if err := app.Close(); err != nil {
					t.Error(err)
				}
			})
			var prior []byte
			key := []byte("data_revision")
			write := func(pause func()) error {
				if fault == "real_new_candidate" {
					manifest.Manifest.BuildDigest = strings.Repeat("b", 64)
					pauseWriter = pause
					armed.Store(true)
					added, reused, err := app.CreateCandidate(secondActor, secondProject, CandidateInput{
						Manifest: manifest.Manifest, BaselineDigest: candidate.BaselineDigest,
					})
					if err == nil && (added.ID == "" || reused || added.ID == candidate.ID) {
						return errors.New("public concurrent writer did not add distinct candidate")
					}
					return err
				}
				return app.db.Update(func(tx *bolt.Tx) error {
					meta := tx.Bucket([]byte(metaBucket))
					prior = bytes.Clone(meta.Get(key))
					if err := meta.Put(key, []byte("{")); err != nil {
						return err
					}
					pause()
					return nil
				})
			}
			type result struct {
				preview Preview
				err     error
			}
			observed := afterOldStoreRead(t, app, write, func() result {
				preview, err := app.PreviewImport(secondActor, secondProject, candidate.ID, f.pending)
				return result{preview, err}
			})
			want, detail := ErrConflict, "evidence changed during preview"
			if fault == "malformed_saved_revision" {
				want, detail = ErrIntegrity, "invalid storage counter"
			}
			if p := observed.preview; p.Token != "" || len(p.Projected) != 0 || len(p.Ordering) != 0 ||
				!errors.Is(observed.err, want) || !strings.Contains(observed.err.Error(), detail) {
				t.Fatalf("preview retained a proposal after the verified old read and revision update: %+v %v", p, observed.err)
			}
			if fault == "malformed_saved_revision" {
				if err := app.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(metaBucket)).Put(key, prior) }); err != nil {
					t.Fatal(err)
				}
			}
			reopened := f.checkReopened(t, want == ErrIntegrity)
			retry, err := reopened.PreviewImport(secondActor, secondProject, candidate.ID, f.pending)
			if err != nil || retry.Token == "" || len(retry.Issues) != 0 || len(retry.Projected) != 1 || retry.Projected[0].LedgerHighWater != 2 {
				t.Fatalf("fresh preview against the current repaired revision failed: %v", err)
			}
		})
	}
}
