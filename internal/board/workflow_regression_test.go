package board_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestRF06DerivedIDRepairPreservesThresholdReview(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	roster := vectorBaseline()
	roster.Behaviors[0].Tests = append(roster.Behaviors[0].Tests, board.TestPlan{ID: "T2", ExpectedOutcome: "No extra access", PlannedAttempts: 1})
	c := fixtureCandidate(t, app, roster)
	stamp := board.FormatUTC(fixtureTime)
	importBytes(t, app, c.ID, csvBytes(t,
		resultRow(c, "T1", "1", "RUN1", "RES1", stamp, stamp, "PASS"),
		resultRow(c, "T2", "1", "RUN1", "RES2", stamp, stamp, "FAIL")))
	derived, err := app.CreateDraft(supervisor, project, roster, board.DraftOptions{FromBaseline: c.BaselineDigest})
	if err != nil {
		t.Fatal(err)
	}
	updated := vectorBaseline()
	updated.Revision = derived.Baseline.Revision
	updated.Behaviors[0].ID = "BEH1-relabel"
	updated.Behaviors[0].Policy.ID = "POL1-relabel"
	updated.Behaviors[0].Policy.MinPassBasisPoints = 5000
	updated.Behaviors[0].Tests[0].ID = "T1-relabel"
	updated.Behaviors[0].Tests = append(updated.Behaviors[0].Tests,
		board.TestPlan{ID: "T2-relabel", ExpectedOutcome: "No extra access", PlannedAttempts: 1})
	if _, err := app.SaveDraft(supervisor, project, derived.ID, derived.Revision, updated); !errors.Is(err, board.ErrInvalid) {
		t.Fatalf("direct derived row rekey returned %v, want invalid", err)
	}
	updated.ID = "standalone-copy"
	updated.Revision = 1
	standalone, err := app.CreateDraft(supervisor, project, updated, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Approve(supervisor, project, standalone.ID, standalone.Revision); !errors.Is(err, board.ErrForbidden) {
		t.Fatalf("equivalent standalone relabel approval returned %v", err)
	}
	if err := app.SetAccount(supervisor, "different", testPassword, []board.Role{board.Reviewer}); err != nil {
		t.Fatal(err)
	}
	approved, err := app.Approve("different", project, standalone.ID, standalone.Revision)
	if err != nil || len(approved.Thresholds) != 1 || approved.Thresholds[0].BehaviorID != "BEH1" {
		t.Fatalf("independent approval lost affected behavior identity: %#v, %v", approved.Thresholds, err)
	}
}

func TestRF08RestartCleansAbandonedPreviewAndPreservesCommittedReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preview-expiry.db")
	now := fixtureTime
	cfg := board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return now }}
	app, err := board.Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Bootstrap(supervisor, testPassword); err != nil {
		t.Fatal(err)
	}
	c := fixtureCandidate(t, app, vectorBaseline())
	at := board.FormatUTC(now)
	raw := csvBytes(t, resultRow(c, "T1", "1", "R1", "R1-1", at, at, "PASS"))
	committable, err := app.PreviewImport(supervisor, project, c.ID, raw)
	if err != nil || committable.Token == "" {
		t.Fatalf("committable preview: %v", err)
	}
	receipt, err := app.CommitImport(supervisor, project, committable.Token, raw)
	if err != nil {
		t.Fatal(err)
	}
	dangling, err := app.PreviewImport(supervisor, project, c.ID, csvBytes(t, runOnly(c, "R2", at)))
	if err != nil || dangling.Token == "" {
		t.Fatalf("staged abandoned preview: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Minute)
	reopened, err := board.Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := reopened.CommitImport(supervisor, project, dangling.Token, nil); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("expired uncommitted binding survived restart: %v", err)
	}
	retry, err := reopened.CommitImport(supervisor, project, committable.Token, nil)
	if err != nil || retry.ID != receipt.ID || retry.LedgerHighWater != 1 {
		t.Fatalf("retained exact replay failed after restart: %#v, %v", retry, err)
	}
}
