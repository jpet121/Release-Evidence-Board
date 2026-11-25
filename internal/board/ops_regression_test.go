package board_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestSTD02ExpiryRecordsAndResumesPartialRemoval(t *testing.T) {
	now := fixtureTime
	app, _ := openBoard(t, func() time.Time { return now })
	directory := t.TempDir()
	backup, err := app.Backup(supervisor, project, directory)
	if err != nil {
		t.Fatal(err)
	}
	sidecar := backup.Path + ".sha256"
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sidecar, 0700); err != nil {
		t.Fatal(err)
	}
	block := filepath.Join(sidecar, "file")
	if err := os.WriteFile(block, []byte("simulated OS removal error"), 0600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * 24 * time.Hour)
	if count, err := app.ExpireBackups(supervisor, project, directory); err == nil || count != 0 {
		t.Fatalf("partial expiry claimed completion: %d, %v", count, err)
	}
	events, err := app.Audit(supervisor, project)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events, func(event board.AuditEvent) bool {
		return event.Action == "backup.expire_intent" && event.Outcome == "success" && event.Actor == supervisor
	}) || !slices.ContainsFunc(events, func(event board.AuditEvent) bool {
		return event.Action == "backup.expire" && event.Outcome == "failure"
	}) {
		t.Fatal("partially removed backup lacks durable intent or failure audit")
	}
	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}
	if count, err := app.ExpireBackups(supervisor, project, directory); err != nil || count != 1 {
		t.Fatalf("retried expiry count %d, error %v", count, err)
	}
	if count, err := app.ExpireBackups(supervisor, project, directory); err != nil || count != 0 {
		t.Fatalf("completed expiry repeated: %d, %v", count, err)
	}
}

func TestSTD10RestoreRequiresBoundChecksum(t *testing.T) {
	for _, metadata := range []string{"missing", "malformed"} {
		t.Run(metadata, func(t *testing.T) {
			app, _ := openBoard(t, func() time.Time { return fixtureTime })
			backup, err := app.Backup(supervisor, project, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			checksumFile := backup.Path + ".sha256"
			switch metadata {
			case "missing":
				err = os.Remove(checksumFile)
			default:
				err = os.WriteFile(checksumFile, []byte("invalid digest"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "restored.db")
			conf := board.Settings{ProjectID: project, ProductID: "DEMO"}
			recovered, err := board.RestoreBackup(backup.Path, destination, conf)
			if err != nil || recovered.IntegrityOK {
				t.Fatalf("restore accepted invalid checksum metadata: %#v, %v", recovered, err)
			}
			restored, err := board.Open(destination, conf)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := restored.Close(); err != nil {
					t.Error(err)
				}
			}()
			if err := restored.Revalidate(supervisor, project); !errors.Is(err, board.ErrIntegrity) {
				t.Fatalf("unbound backup resumed assessment: %v", err)
			}
		})
	}
}

func TestAC01203MaterialRejectionsAreAttributedWithoutPayload(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	if err := app.SetAccount(supervisor, "viewer", testPassword, []board.Role{board.Viewer}); err != nil {
		t.Fatal(err)
	}
	roster := vectorBaseline()
	draft, err := app.CreateDraft(supervisor, project, roster, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.SaveDraft(supervisor, project, draft.ID, draft.Revision+1, roster)
	if !errors.Is(err, board.ErrConflict) {
		t.Fatalf("stale revision = %v", err)
	}
	if _, err := app.Approve("viewer", project, draft.ID, draft.Revision); !errors.Is(err, board.ErrForbidden) {
		t.Fatalf("viewer baseline approval = %v", err)
	}
	_, _, err = app.CreateCandidate("viewer", project, board.CandidateInput{DisplayVersion: "private attempted text"})
	if !errors.Is(err, board.ErrForbidden) {
		t.Fatalf("viewer candidate creation = %v", err)
	}
	_, err = app.CommitImport(supervisor, project, "secret preview identifier", nil)
	if !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("unbound preview = %v", err)
	}
	events, err := app.Audit(supervisor, project)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []struct{ action, actor, outcome string }{
		{"draft.edit", supervisor, "failure"}, {"baseline.approve", "viewer", "denied"},
		{"candidate.create", "viewer", "denied"}, {"import.commit", supervisor, "denied"},
	} {
		if !slices.ContainsFunc(events, func(actual board.AuditEvent) bool {
			return actual.Action == required.action && actual.Actor == required.actor && actual.Outcome == required.outcome &&
				actual.Time.Equal(fixtureTime) && len(actual.References) == 0
		}) {
			t.Errorf("missing attributed rejection audit for %s", required.action)
		}
	}
}
