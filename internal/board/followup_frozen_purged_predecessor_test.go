package board_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupFrozenSuccessorLabelsPurgedPredecessorAndKeepsSharedManifest(t *testing.T) {
	now := fixtureTime
	app, _ := openBoard(t, func() time.Time { return now })
	predecessor := fixtureCandidate(t, app, vectorBaseline())
	now = now.Add(24 * time.Hour)
	next := vectorBaseline()
	next.Revision++
	next.Behaviors[0].Revision++
	next.Behaviors[0].Text = "Record whether the new approval preserves the shared source manifest"
	successor := fixtureCandidate(t, app, next)
	if successor.PredecessorID != predecessor.ID || successor.ManifestDigest != predecessor.ManifestDigest ||
		successor.BaselineDigest == predecessor.BaselineDigest {
		t.Fatal("fixture did not bind the same manifest to a distinct approved successor")
	}
	first, err := app.Freeze(supervisor, project, successor.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, earlier, err := app.ReportHTML(supervisor, project, first.ID)
	beforeText := []byte("<dt>Predecessor</dt><dd>" + predecessor.ID + "</dd>")
	if err != nil || !bytes.Contains(earlier, beforeText) {
		t.Fatalf("report before purge lost its retained predecessor: %v", err)
	}
	now = fixtureTime.Add(180 * 24 * time.Hour)
	scope := []string{predecessor.ID}
	plan, err := app.PreviewPurge(supervisor, project, scope)
	if err != nil || len(plan.Ineligible) != 0 || len(plan.SharedBlockers) != 0 || len(plan.OrphanManifests) != 0 {
		t.Fatalf("purge would remove the manifest still used by the successor: %v, %v", plan.OrphanManifests, err)
	}
	if _, err := app.Purge(supervisor, project, scope, plan.Confirmation); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := app.Candidate(supervisor, project, predecessor.ID); !errors.Is(err, board.ErrNotFound) {
		t.Errorf("eligible predecessor remains after purge: %v", err)
	}
	_, manifest, _, err := app.Candidate(supervisor, project, successor.ID)
	if err != nil || manifest.Descriptor.Digest != predecessor.ManifestDigest {
		t.Fatalf("purge lost the successor's checked shared manifest: %v", err)
	}
	second, err := app.Freeze(supervisor, project, successor.ID)
	if err != nil || second.ID == first.ID || second.Status != board.Blocked {
		t.Fatalf("successor could not freeze after predecessor purge: %+v, %v", second, err)
	}
	_, later, err := app.ReportHTML(supervisor, project, second.ID)
	afterText := []byte("<dt>Predecessor</dt><dd>" + predecessor.ID + " (purged record)</dd>")
	if err != nil || !bytes.Contains(later, afterText) {
		t.Errorf("new frozen report lost the exact purged predecessor annotation: %v", err)
	}
	if report, unchanged, err := app.ReportHTML(supervisor, project, first.ID); err != nil || report != first || !bytes.Equal(earlier, unchanged) {
		t.Errorf("purge changed the report frozen before it: %v", err)
	}
}
