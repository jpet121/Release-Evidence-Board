package board_test

import (
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupOperatorMutationsRespectViewerAndProject(t *testing.T) {
	const reader = "readonly-user"
	now := fixtureTime
	app, _ := openBoard(t, func() time.Time { return now })
	if err := app.SetAccount(supervisor, reader, testPassword, []board.Role{board.Viewer}); err != nil {
		t.Fatal(err)
	}
	original := fixtureCandidate(t, app, vectorBaseline())
	now = fixtureTime.Add(180 * 24 * time.Hour)
	plan, err := app.PreviewPurge(supervisor, project, []string{original.ID})
	if err != nil || len(plan.Ineligible) != 0 {
		t.Fatalf("positive purge fixture is not eligible: %v", err)
	}
	before, err := app.Candidates(supervisor, project)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("AC-001-02.P009", func(t *testing.T) {
		if err := app.SetAccount(reader, "unexpected-reviewer", testPassword, []board.Role{board.Reviewer}); !errors.Is(err, board.ErrForbidden) {
			t.Fatalf("viewer assigned reviewer access: %v", err)
		}
		if _, err := app.Account("unexpected-reviewer"); !errors.Is(err, board.ErrNotFound) {
			t.Fatalf("refused grant inserted an account: %v", err)
		}
		recordApprovalInstance(t, "AC-001-02.P009", map[string]any{"requiredOperation": "access assignment"},
			"Every mutation is denied and data is unchanged")
	})

	for _, item := range []struct {
		actor, scope, backupID, purgeID, expected string
		denial                                    error
	}{
		{reader, project, "AC-001-02.P010", "AC-001-02.P013", "Every mutation is denied and data is unchanged", board.ErrForbidden},
		{supervisor, "foreign", "AC-001-03.P024", "AC-001-03.P027", "No foreign data or mutation is exposed", board.ErrNotFound},
	} {
		t.Run(item.backupID, func(t *testing.T) {
			dir := t.TempDir()
			backup, err := app.Backup(item.actor, item.scope, dir)
			if !errors.Is(err, item.denial) || backup.Path != "" || backup.SHA256 != "" {
				t.Fatalf("denied backup exposed contents or references: %v", err)
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 0 {
				t.Fatalf("denied backup created retained output files: %v", err)
			}
			recordApprovalInstance(t, item.backupID, map[string]any{"requiredOperation": "backup"}, item.expected)
		})
		t.Run(item.purgeID, func(t *testing.T) {
			scope := []string{original.ID}
			preview, err := app.PreviewPurge(item.actor, item.scope, scope)
			if !errors.Is(err, item.denial) || preview.Confirmation != "" || len(preview.CandidateIDs) != 0 {
				t.Fatalf("denied purge preview disclosed protected scope: %v", err)
			}
			removed, err := app.Purge(item.actor, item.scope, scope, plan.Confirmation)
			if !errors.Is(err, item.denial) || len(removed.CandidateIDs) != 0 {
				t.Fatalf("denied actor purged a valid reviewer-issued scope: %v", err)
			}
			after, err := app.Candidates(supervisor, project)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("denied purge changed the eligible candidate and scope: %v", err)
			}
			recordApprovalInstance(t, item.purgeID, map[string]any{"requiredOperation": "expiry purge"}, item.expected)
		})
	}
}
