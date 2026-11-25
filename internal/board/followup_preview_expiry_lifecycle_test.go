package board_test

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupImportPreviewLifetimeAndCancelPreserveExactReceipts(t *testing.T) {
	for _, entry := range []struct {
		name string
		age  time.Duration
		live bool
	}{
		{"one microsecond before expiry", 30*time.Minute - time.Microsecond, true},
		{"exact expiry", 30 * time.Minute, false},
	} {
		t.Run(entry.name, func(t *testing.T) {
			now := fixtureTime
			app, _ := openBoard(t, func() time.Time { return now })
			candidate := fixtureCandidate(t, app, vectorBaseline())
			stamp := board.FormatUTC(now)
			raw := csvBytes(t, resultRow(candidate, "T1", "1", "PREVIEW-AGE", "RESULT-AGE", stamp, stamp, board.Pass))
			if preview, err := app.PreviewImport(supervisor, project, "C-absent", raw); !errors.Is(err, board.ErrNotFound) || preview.Token != "" {
				t.Errorf("missing open candidate accepted a staged upload: %v", err)
			}
			if err := app.CancelPreview(supervisor, project, "absent-preview-token"); !errors.Is(err, board.ErrNotFound) {
				t.Errorf("missing token reported cancellation success: %v", err)
			}
			old, err := app.Freeze(supervisor, project, candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, html, err := app.ReportHTML(supervisor, project, old.ID)
			if err != nil || len(html) == 0 {
				t.Fatalf("prior frozen artifact is unavailable: %v", err)
			}
			preview := previewFollowupFile(t, app, candidate.ID, raw)
			oversize := make([]byte, board.MaxCSVBytes+1)
			if received, err := app.CommitImport(supervisor, project, preview.Token, oversize); !errors.Is(err, board.ErrConflict) || received.ID != "" {
				t.Errorf("oversize body replaced verified staged original: %v", err)
			}
			now = now.Add(entry.age)
			receipt, err := app.CommitImport(supervisor, project, preview.Token, nil)
			if entry.live {
				if err != nil || receipt.LedgerHighWater != 1 {
					t.Fatalf("live preview could not confirm exact source: %v", err)
				}
				if err := app.CancelPreview(supervisor, project, preview.Token); err != nil {
					t.Fatal(err)
				}
				now = now.Add(time.Microsecond)
				if retry, err := app.CommitImport(supervisor, project, preview.Token, nil); err != nil || !reflect.DeepEqual(retry, receipt) {
					t.Errorf("cancelling staging damaged the already committed exact retry: %v", err)
				}
				if original, err := app.SourceCSV(supervisor, project, receipt.ID); err != nil || !bytes.Equal(raw, original) {
					t.Errorf("exact committed original was not retained: %v", err)
				}
			} else {
				if !errors.Is(err, board.ErrConflict) || receipt.ID != "" {
					t.Errorf("uncommitted preview accepted at the exact expiry: %v", err)
				}
				if assessment, err := app.Assess(supervisor, project, candidate.ID); err != nil || assessment.LedgerHighWater != 0 {
					t.Errorf("expired confirmation changed the visible ledger: %v", err)
				}
			}
			if current, saved, err := app.ReportHTML(supervisor, project, old.ID); err != nil || current != old || !bytes.Equal(saved, html) {
				t.Errorf("staging lifecycle changed independently retained HTML: %v", err)
			}
		})
	}
}
