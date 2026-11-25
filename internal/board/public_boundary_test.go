package board_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestPublicEvidenceOperationsRejectUnauthorizedActorsBeforeInterpretingPayload(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	c := fixtureCandidate(t, app, vectorBaseline())
	if err := app.SetAccount(supervisor, "viewer", testPassword, []board.Role{board.Viewer}); err != nil {
		t.Fatal(err)
	}
	if err := app.SetAccount(supervisor, "operator-only", testPassword, []board.Role{board.Operator}); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		actor, selectedProject string
		want                   error
	}{
		{"viewer", project, board.ErrForbidden},
		{supervisor, "foreign", board.ErrNotFound},
	} {
		p, err := app.PreviewImport(check.actor, check.selectedProject, c.ID, []byte("malformed test result schema"))
		if !errors.Is(err, check.want) || p.Token != "" || p.Hash != "" || len(p.Issues) != 0 || len(p.Projected) != 0 {
			t.Fatalf("unauthorized preview interpreted payload: %v", err)
		}
	}
	if held, err := app.AssessmentSuspended("operator-only", project); held || !errors.Is(err, board.ErrForbidden) {
		t.Fatalf("operator without read role queried evidence status: %t, %v", held, err)
	}
	if held, err := app.AssessmentSuspended("viewer", "foreign"); held || !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("viewer queried a different project's evidence status: %t, %v", held, err)
	}
	if _, _, _, err := app.Candidate("viewer", project, "C-unavailable"); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("missing candidate did not produce scoped not-found: %v", err)
	}
}

func TestPublicAssessmentHonorsPersistedOperatorHold(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	c := fixtureCandidate(t, app, vectorBaseline())
	stamp := board.FormatUTC(fixtureTime)
	importBytes(t, app, c.ID, csvBytes(t, resultRow(c, "T1", "1", "RUN1", "RES1", stamp, stamp, "PASS")))
	initial, err := app.Assess(supervisor, project, c.ID)
	if err != nil || initial.Status != board.Sufficient {
		t.Fatalf("fixture lacks sufficient evidence before suspension: %v", err)
	}
	for _, denied := range []struct {
		actor, project string
		want           error
	}{
		{"unassigned-account", project, board.ErrForbidden},
		{"viewer", "foreign", board.ErrNotFound},
	} {
		private, denial := app.Assess(denied.actor, denied.project, c.ID)
		if private.Status != "" || private.Candidate.ID != "" || !errors.Is(denial, denied.want) {
			t.Fatalf("assessment disclosed evidence to an unauthorized actor: %q, %v", private.Status, denial)
		}
	}
	if err := app.SuspendAssessments(supervisor, project); err != nil {
		t.Fatal(err)
	}
	suspended, err := app.Assess(supervisor, project, c.ID)
	if suspended.Status != "" || !errors.Is(err, board.ErrIntegrity) {
		t.Fatalf("suspended assessment returned %q or inappropriate error %v", suspended.Status, err)
	}
}

func TestDraftCreationRejectsInvalidIdentityAndOversizedRoster(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	for _, check := range []struct {
		name   string
		change func(*board.Baseline)
	}{
		{"empty_id", func(roster *board.Baseline) { roster.ID = "" }},
		{"overlong_id", func(roster *board.Baseline) { roster.ID = strings.Repeat("a", 81) }},
		{"invalid_revision", func(roster *board.Baseline) { roster.Revision = -1 }},
		{"exceeds_behavior_count", func(roster *board.Baseline) { roster.Behaviors = capacityRoster(501, 1) }},
	} {
		t.Run(check.name, func(t *testing.T) {
			baseline := vectorBaseline()
			check.change(&baseline)
			if draft, err := app.CreateDraft(supervisor, project, baseline, board.DraftOptions{}); draft.ID != "" || !errors.Is(err, board.ErrInvalid) {
				t.Fatalf("invalid draft generated %q: %v", draft.ID, err)
			}
		})
	}
	_, saved, err := app.Baselines(supervisor, project)
	if err != nil || len(saved) != 0 {
		t.Fatalf("invalid draft operation left material records: %v", err)
	}
}

func TestFrozenReportIdentifiesExactPredecessor(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	before := fixtureCandidate(t, app, vectorBaseline())
	manifest := vectorManifest()
	manifest.BuildDigest = strings.Repeat("a", 64)
	corrected, reused, err := app.CreateCandidate(supervisor, project, board.CandidateInput{
		Manifest: manifest, BaselineDigest: before.BaselineDigest, PredecessorID: before.ID, Correction: "Synthetic rebuild",
	})
	if err != nil || reused {
		t.Fatalf("correction did not produce a separate candidate: %v", err)
	}
	report, err := app.Freeze(supervisor, project, corrected.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, html, err := app.ReportHTML(supervisor, project, report.ID)
	if err != nil || !bytes.Contains(html, []byte("<dt>Predecessor</dt><dd>"+before.ID+"</dd>")) {
		t.Fatal("frozen report omitted the exact predecessor")
	}
}
