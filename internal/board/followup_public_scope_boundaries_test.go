package board

import (
	"errors"
	"strings"
	"testing"
)

func TestFollowupPublicEvidenceMethodsRefuseDifferentWorkspace(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	_, _, approval, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := f.app.CreateDraft(secondActor, secondProject, approval.Baseline, DraftOptions{FromBaseline: approval.Descriptor.Digest})
	if err != nil {
		t.Fatal(err)
	}
	backup, err := f.app.Backup(secondActor, secondProject, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const otherProject = "another-synthetic-workspace"
	cases := []struct {
		name string
		call func() error
	}{
		{"approved roster", func() error {
			_, err := f.app.Baseline(secondActor, otherProject, approval.Descriptor.Digest)
			return err
		}},
		{"draft read", func() error { _, err := f.app.Draft(secondActor, otherProject, draft.ID); return err }},
		{"draft save", func() error {
			_, err := f.app.SaveDraft(secondActor, otherProject, draft.ID, draft.Revision, draft.Baseline)
			return err
		}},
		{"threshold review", func() error { _, err := f.app.ReviewThresholds(secondActor, otherProject, draft.ID); return err }},
		{"candidate catalog", func() error { _, err := f.app.Candidates(secondActor, otherProject); return err }},
		{"receipt", func() error { _, err := f.app.Receipt(secondActor, otherProject, f.receipt.ID); return err }},
		{"exact original", func() error { _, err := f.app.SourceCSV(secondActor, otherProject, f.receipt.ID); return err }},
		{"audit", func() error { _, err := f.app.Audit(secondActor, otherProject); return err }},
		{"restore drill", func() error { return f.app.RecordRestoreDrill(secondActor, otherProject, backup.Path) }},
		{"suspension", func() error { return f.app.SuspendAssessments(secondActor, otherProject) }},
	}
	for _, entry := range cases {
		t.Run(entry.name, func(t *testing.T) {
			if err := entry.call(); !errors.Is(err, ErrNotFound) {
				t.Errorf("public operation accepted a different project: %v", err)
			}
		})
	}
	if report, err := f.app.Freeze(secondActor, secondProject, "C-absent"); !errors.Is(err, ErrNotFound) || report.ID != "" {
		t.Errorf("authorized freeze accepted nonexistent candidate: %v", err)
	}
	for _, id := range []string{"", "X-" + f.receipt.Hash, "I-" + strings.Repeat("z", 64)} {
		if receipt, err := f.app.Receipt(secondActor, secondProject, id); !errors.Is(err, ErrNotFound) || receipt.ID != "" {
			t.Errorf("malformed receipt identity returned durable evidence: %q, %v", receipt.ID, err)
		}
		if original, err := f.app.SourceCSV(secondActor, secondProject, id); !errors.Is(err, ErrNotFound) || len(original) != 0 {
			t.Errorf("malformed receipt identity returned original bytes: %d, %v", len(original), err)
		}
	}
	app := f.checkReopened(t, false)
	if current, err := app.Draft(secondActor, secondProject, draft.ID); err != nil || current.Revision != draft.Revision {
		t.Errorf("refused workspace operation changed owned draft revision: %v", err)
	}
	if current, err := app.Health(secondActor, secondProject); err != nil || !current.LastDrill.IsZero() || current.Hold {
		t.Errorf("refused workspace operation changed recovery state: %v", err)
	}
}
