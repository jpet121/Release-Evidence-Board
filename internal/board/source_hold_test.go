package board

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestSourceDownloadIntegrityFaultSuspendsOtherCandidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.db")
	at, err := time.Parse(time.RFC3339, "2026-11-17T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(path, Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := b.Bootstrap("reviewer", "synthetic-hold-password"); err != nil {
		t.Fatal(err)
	}
	draft, err := b.CreateDraft("reviewer", "pilot", Baseline{ProductID: "DEMO", ID: "B1", Revision: 1,
		Behaviors: []Behavior{{ID: "BH", Revision: 1, Required: true, Text: "A required behavior",
			Policy: Policy{ID: "P1", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
			Tests:  []TestPlan{{ID: "T1", ExpectedOutcome: "Pass condition", MustPass: true, PlannedAttempts: 1}}}},
	}, DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := b.Approve("reviewer", "pilot", draft.ID, draft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	type candidateEvidence struct {
		c    Candidate
		p    Preview
		r    Receipt
		data []byte
	}
	create := func(mark string) candidateEvidence {
		t.Helper()
		c, _, err := b.CreateCandidate("reviewer", "pilot", CandidateInput{
			Manifest: Manifest{ProductID: "DEMO", ReleaseID: "Release-" + mark, ApplicationVersion: "1",
				BuildDigest: strings.Repeat(mark, 64), EnvironmentID: "E1", PromptDigest: "not_applicable",
				ModelConfigDigest: "not_applicable", KnowledgeDigest: "not_applicable"},
			BaselineDigest: approved.Descriptor.Digest,
		})
		if err != nil {
			t.Fatal(err)
		}
		raw := []byte(strings.Join(resultColumns[:], ",") + "\n" + fmt.Sprintf(
			"reb-results/1.3,RESULT,RES-%s,DEMO,Release-%s,%s,%s,BH,1,P1,1,T1,RUN-%s,1,%s,%s,PASS,,\n",
			mark, mark, c.ManifestDigest, c.BaselineDigest, mark, FormatUTC(at), FormatUTC(at)))
		p, err := b.PreviewImport("reviewer", "pilot", c.ID, raw)
		if err != nil || len(p.Issues) != 0 || p.Token == "" {
			t.Fatalf("fixture preview issues: %v %v", p.Issues, err)
		}
		r, err := b.CommitImport("reviewer", "pilot", p.Token, raw)
		if err != nil {
			t.Fatal(err)
		}
		return candidateEvidence{c: c, p: p, r: r, data: raw}
	}
	damaged, healthy := create("a"), create("b")
	inGoodStanding, err := b.Assess("reviewer", "pilot", healthy.c.ID)
	if err != nil || inGoodStanding.Status != Sufficient {
		t.Fatalf("before damage = %s %v", inGoodStanding.Status, err)
	}
	// Change only the original CSV in the real store while the service is alive.
	if err := b.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(csvBucket)).Put([]byte(damaged.r.Hash), []byte("simulated media fault"))
	}); err != nil {
		t.Fatal(err)
	}
	if raw, err := b.SourceCSV("reviewer", "pilot", damaged.r.ID); len(raw) != 0 || !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt source returned %d bytes and %v", len(raw), err)
	}
	if _, err := b.Assess("reviewer", "pilot", healthy.c.ID); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("unaffected candidate assessment escaped hold: %v", err)
	}
	if _, err := b.Freeze("reviewer", "pilot", healthy.c.ID); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("unaffected candidate freeze escaped hold: %v", err)
	}
	if held, err := b.AssessmentSuspended("reviewer", "pilot"); err != nil || !held {
		t.Fatalf("retained read-only state lacks hold: %v %v", held, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := b.CommitImport("reviewer", "pilot", healthy.p.Token, healthy.data)
	if err != nil || replay.ID != healthy.r.ID {
		t.Fatalf("exact replay under hold = %s %v", replay.ID, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("exact replay under hold changed database bytes: %v", err)
	}
}
