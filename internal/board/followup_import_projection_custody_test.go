package board

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupMixedPreviewRejectsUnverifiedOpenCandidate(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	_, _, baseline, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	open := reviewCandidate(t, f.app, baseline, "b")
	key := []byte(open.ID)
	var original []byte
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		bindings := tx.Bucket([]byte(candidateMetaBucket))
		original = bytes.Clone(bindings.Get(key))
		if len(original) == 0 {
			return errors.New("no original open candidate binding")
		}
		return bindings.Put(key, []byte(strings.Repeat("f", 64)))
	}); err != nil {
		t.Fatal(err)
	}
	p, err := f.app.PreviewImport(secondActor, secondProject, open.ID, f.pending)
	if !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "retained metadata differs from its integrity binding") ||
		p.Token != "" || p.Projected != nil || p.Ordering != nil {
		t.Errorf("mixed preview published without exact open candidate custody: token=%t, projected=%d, %v", p.Token != "", len(p.Projected), err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(candidateMetaBucket)).Put(key, original)
	}); err != nil {
		t.Fatal(err)
	}
	app := f.checkReopened(t, true)
	p, err = app.PreviewImport(secondActor, secondProject, open.ID, f.pending)
	if err != nil || p.Token == "" || len(p.Issues) != 0 || len(p.Projected) != 2 {
		t.Fatalf("repaired mixed preview failed: projections=%d, %v", len(p.Projected), err)
	}
	statuses := make(map[string]string)
	for _, assessment := range p.Projected {
		statuses[assessment.Candidate.ID] = assessment.Status
	}
	if statuses[f.candidate.ID] != Sufficient || statuses[open.ID] != Blocked {
		t.Errorf("repaired mixed preview has wrong candidate assessments: %v", statuses)
	}
}
