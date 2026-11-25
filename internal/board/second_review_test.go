package board

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

const secondActor = "reviewer"
const secondProject = "pilot"

func reviewBoard(t *testing.T) (*Board, ApprovedBaseline, time.Time) {
	t.Helper()
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	app, err := Open(filepath.Join(t.TempDir(), "evidence.db"), Settings{
		ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := app.Bootstrap(secondActor, "synthetic-review-password"); err != nil {
		t.Fatal(err)
	}
	roster := Baseline{ProductID: "DEMO", ID: "B1", Revision: 1, Behaviors: []Behavior{{
		ID: "BH", Revision: 1, Required: true, Text: "Behavior used in the review fixture",
		Policy: Policy{ID: "P1", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
		Tests: []TestPlan{
			{ID: "T1", ExpectedOutcome: "Required result", MustPass: true, PlannedAttempts: 1},
			{ID: "T2", ExpectedOutcome: "Additional result", PlannedAttempts: 1},
		},
	}}}
	draft, err := app.CreateDraft(secondActor, secondProject, roster, DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := app.Approve(secondActor, secondProject, draft.ID, draft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	return app, baseline, at
}

func reviewCandidate(t *testing.T, b *Board, baseline ApprovedBaseline, mark string) Candidate {
	t.Helper()
	candidate, reused, err := b.CreateCandidate(secondActor, secondProject, CandidateInput{
		Manifest: Manifest{ProductID: "DEMO", ReleaseID: "release-" + mark, ApplicationVersion: "same version", BuildDigest: strings.Repeat(mark, 64),
			PromptDigest: "not_applicable", ModelConfigDigest: "not_applicable", KnowledgeDigest: "not_applicable", EnvironmentID: "E1"},
		BaselineDigest: baseline.Descriptor.Digest,
	})
	if err != nil || reused {
		t.Fatalf("create fixture candidate = %v, reused %v", err, reused)
	}
	return candidate
}

func reviewRows(c Candidate, mark, name, outcome string, when time.Time) string {
	stamp := FormatUTC(when)
	prefix := fmt.Sprintf("reb-results/1.3,RESULT,RES%s-", name)
	body := fmt.Sprintf(",DEMO,release-%s,%s,%s,BH,1,P1,1,", mark, c.ManifestDigest, c.BaselineDigest)
	return prefix + "1" + body + "T1," + name + ",1," + stamp + "," + stamp + ",PASS,,\n" +
		prefix + "2" + body + "T2," + name + ",1," + stamp + "," + stamp + "," + outcome + ",,\n"
}

func reviewCommit(t *testing.T, b *Board, candidate Candidate, rows string) (Receipt, Preview, []byte) {
	t.Helper()
	raw := []byte(strings.Join(resultColumns[:], ",") + "\n" + rows)
	preview, err := b.PreviewImport(secondActor, secondProject, candidate.ID, raw)
	if err != nil || len(preview.Issues) != 0 {
		t.Fatalf("preview fixture: %v, %#v", err, preview.Issues)
	}
	receipt, err := b.CommitImport(secondActor, secondProject, preview.Token, raw)
	if err != nil {
		t.Fatal(err)
	}
	return receipt, preview, raw
}

func TestExactReplayRejectsCorruptedRetainedReceipt(t *testing.T) {
	for _, damage := range []string{"id", "empty_id", "hash", "missing_bytes", "moved_key"} {
		t.Run(damage, func(t *testing.T) {
			b, baseline, at := reviewBoard(t)
			c := reviewCandidate(t, b, baseline, "a")
			r, p, raw := reviewCommit(t, b, c, reviewRows(c, "a", "RUN1", "PASS", at))
			if err := b.db.Update(func(tx *bolt.Tx) error {
				switch damage {
				case "id":
					r.ID = "I-" + strings.Repeat("b", 64)
				case "empty_id":
					r.ID = ""
				case "hash":
					key := r.Hash
					r.Hash = strings.Repeat("a", 64)
					return putJSON(tx, receiptBucket, key, r)
				case "missing_bytes":
					return tx.Bucket([]byte(csvBucket)).Delete([]byte(r.Hash))
				case "moved_key":
					return moveTestKey(tx, receiptBucket, r.Hash, strings.Repeat("b", 64))
				}
				return putJSON(tx, receiptBucket, r.Hash, r)
			}); err != nil {
				t.Fatal(err)
			}
			retry, err := b.CommitImport(secondActor, secondProject, p.Token, raw)
			if !errors.Is(err, ErrIntegrity) || retry.ID != "" {
				t.Errorf("replay returned %q and %v from damaged receipt", retry.ID, err)
			}
			held, err := b.AssessmentSuspended(secondActor, secondProject)
			if err != nil || !held {
				t.Errorf("corrupt receipt did not suspend assessment: %v, %v", held, err)
			}
		})
	}
}

func moveTestKey(tx *bolt.Tx, bucket, key, replacement string) error {
	store := tx.Bucket([]byte(bucket))
	value := store.Get([]byte(key))
	if err := store.Put([]byte(replacement), value); err != nil {
		return err
	}
	return store.Delete([]byte(key))
}

func TestRevalidationRejectsMiskeyedPrimaryRecords(t *testing.T) {
	for _, bucket := range []string{receiptBucket, runBucket, draftBucket, csvBucket, manifestBucket, candidateBucket, baselineBucket, materialBucket} {
		t.Run(bucket, func(t *testing.T) {
			b, baseline, at := reviewBoard(t)
			c := reviewCandidate(t, b, baseline, "a")
			r, _, _ := reviewCommit(t, b, c, reviewRows(c, "a", "RUN1", "PASS", at))
			key := map[string]string{receiptBucket: r.Hash, runBucket: "RUN1", draftBucket: baseline.DraftID, csvBucket: r.Hash,
				manifestBucket: c.ManifestDigest, candidateBucket: c.ID, baselineBucket: c.BaselineDigest, materialBucket: c.MaterialDigest}[bucket]
			if err := b.db.Update(func(tx *bolt.Tx) error { return moveTestKey(tx, bucket, key, "moved") }); err != nil {
				t.Fatal(err)
			}
			if err := b.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("revalidated miskeyed %s: %v", bucket, err)
			}
		})
	}
}

func TestFrozenMetadataMustMatchItsDurableBinding(t *testing.T) {
	for _, damage := range []string{"status", "assessment_time", "missing_metadata_digest", "moved_key"} {
		t.Run(damage, func(t *testing.T) {
			b, baseline, _ := reviewBoard(t)
			c := reviewCandidate(t, b, baseline, "a")
			record, err := b.Freeze(secondActor, secondProject, c.ID)
			if err != nil || record.Status != Blocked {
				t.Fatalf("initial fixture report %s: %v", record.Status, err)
			}
			_, original, err := b.ReportHTML(secondActor, secondProject, record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.db.Update(func(tx *bolt.Tx) error {
				switch damage {
				case "status":
					record.Status = Sufficient
				case "assessment_time":
					record.AssessedAt = record.AssessedAt.Add(time.Hour)
				case "missing_metadata_digest":
					return tx.Bucket([]byte(reportMetaBucket)).Delete([]byte(record.ID))
				default:
					return moveTestKey(tx, reportBucket, record.ID, "RPT-moved")
				}
				return putJSON(tx, reportBucket, record.ID, record)
			}); err != nil {
				t.Fatal(err)
			}
			list, err := b.Reports(secondActor, secondProject, c.ID)
			if len(list) > 0 || !errors.Is(err, ErrIntegrity) {
				t.Errorf("historical listing returned corrupt metadata %#v: %v", list, err)
			}
			safe, raw, err := b.ReportHTML(secondActor, secondProject, record.ID)
			if damage == "missing_metadata_digest" {
				if len(raw) > 0 || !errors.Is(err, ErrIntegrity) {
					t.Errorf("unbound historical HTML was returned: %v", err)
				}
			} else if err != nil || !safe.IntegrityWarning || safe.Status != "" || !bytes.Equal(raw, original) {
				t.Errorf("safe historical HTML recovery failed: warning=%t, status=%q, error=%v", safe.IntegrityWarning, safe.Status, err)
			}
			if err := b.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
				t.Errorf("revalidated corrupt frozen record: %v", err)
			}
		})
	}
}

func TestPreviewCannotStageAfterConcurrentHold(t *testing.T) {
	b, baseline, at := reviewBoard(t)
	c := reviewCandidate(t, b, baseline, "a")
	raw := []byte(strings.Join(resultColumns[:], ",") + "\n" + reviewRows(c, "a", "RUN1", "PASS", at))
	writer, err := b.db.Begin(true)
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
	previous := b.db.Stats().TxN
	type attempt struct {
		value Preview
		err   error
	}
	result := make(chan attempt, 1)
	go func() {
		p, err := b.PreviewImport(secondActor, secondProject, c.ID, raw)
		result <- attempt{p, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for b.db.Stats().TxN == previous {
		if time.Now().After(deadline) {
			t.Fatal("preview did not begin its reader snapshot")
		}
		runtime.Gosched()
	}
	if err := writer.Bucket([]byte(metaBucket)).Put([]byte("assessment_hold"), []byte("incident")); err != nil {
		t.Fatal(err)
	}
	err = writer.Commit()
	writer = nil
	if err != nil {
		t.Fatal(err)
	}
	got := <-result
	if !errors.Is(got.err, ErrIntegrity) || got.value.Token != "" || len(got.value.Projected) != 0 {
		t.Fatalf("preview escaped committed hold with token=%t projection=%d err=%v", got.value.Token != "", len(got.value.Projected), got.err)
	}
}

func TestMixedPreviewUsesSingleProductHighWater(t *testing.T) {
	b, baseline, at := reviewBoard(t)
	a, c := reviewCandidate(t, b, baseline, "a"), reviewCandidate(t, b, baseline, "b")
	raw := []byte(strings.Join(resultColumns[:], ",") + "\n" + reviewRows(a, "a", "AAA", "PASS", at) + reviewRows(c, "b", "BBB", "PASS", at))
	preview, err := b.PreviewImport(secondActor, secondProject, a.ID, raw)
	if err != nil || len(preview.Projected) != 2 {
		t.Fatalf("mixed preview: %v, %#v", err, preview.Issues)
	}
	for _, projected := range preview.Projected {
		if projected.LedgerHighWater != 2 {
			t.Errorf("candidate %s reports provisional product high-water %d; want whole import 2", projected.Candidate.ID, projected.LedgerHighWater)
		}
	}
}

func TestThresholdReviewCatchesRenamedReplacementOfOptionalizedBehavior(t *testing.T) {
	b, baseline, at := reviewBoard(t)
	c := reviewCandidate(t, b, baseline, "a")
	reviewCommit(t, b, c, reviewRows(c, "a", "RUN1", "FAIL", at))
	roster := baseline.Baseline
	roster.Behaviors[0].Required = false
	replacement := roster.Behaviors[0]
	replacement.ID, replacement.Required = "renamed", true
	replacement.Policy.ID, replacement.Policy.MinPassBasisPoints = "renamed-policy", 5000
	replacement.Tests = append([]TestPlan(nil), replacement.Tests...)
	replacement.Tests[0].ID, replacement.Tests[1].ID = "T1-renamed", "T2-renamed"
	roster.Behaviors = append(roster.Behaviors, replacement)
	draft, err := b.CreateDraft(secondActor, secondProject, roster, DraftOptions{FromBaseline: baseline.Descriptor.Digest})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Approve(secondActor, secondProject, draft.ID, draft.Revision); !errors.Is(err, ErrForbidden) {
		t.Fatalf("same reducer approved a lowered alias that replaced the inherited required behavior: %v", err)
	}
	roster.Behaviors[1].Tests[1].ExpectedOutcome = "Different separately approved behavior"
	distinct, err := b.CreateDraft(secondActor, secondProject, roster, DraftOptions{FromBaseline: baseline.Descriptor.Digest})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Approve(secondActor, secondProject, distinct.ID, distinct.Revision); err != nil {
		t.Fatalf("a genuinely different behavior unexpectedly inherited the guard: %v", err)
	}
}
