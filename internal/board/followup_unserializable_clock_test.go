package board

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFollowupUnserializableTimeRollsBackPublicEvidenceWrites(t *testing.T) {
	for _, action := range []string{"preview", "candidate", "report"} {
		t.Run(action, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			path := f.app.db.Path()
			if err := f.app.Close(); err != nil {
				t.Fatal(err)
			}
			armed := false
			app, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time {
				if armed {
					armed = false
					return time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
				}
				return f.at
			}})
			if err != nil {
				t.Fatal(err)
			}
			originalApp := app
			t.Cleanup(func() {
				if err := originalApp.Close(); err != nil {
					t.Error(err)
				}
			})
			_, stored, _, err := app.Candidate(secondActor, secondProject, f.candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			manifest := stored.Manifest
			manifest.BuildDigest = strings.Repeat("b", 64)
			input := CandidateInput{Manifest: manifest, BaselineDigest: f.candidate.BaselineDigest}
			armed = true
			var operationError error
			switch action {
			case "preview":
				preview, err := app.PreviewImport(secondActor, secondProject, f.candidate.ID, f.pending)
				operationError = err
				if preview.Token != "" || preview.Projected != nil || preview.Ordering != nil {
					t.Error("serialization failure published a usable preview")
				}
			case "candidate":
				_, _, operationError = app.CreateCandidate(secondActor, secondProject, input)
			case "report":
				_, operationError = app.Freeze(secondActor, secondProject, f.candidate.ID)
			}
			var marshalErr *json.MarshalerError
			if armed || !errors.As(operationError, &marshalErr) || !strings.Contains(marshalErr.Error(), "year outside") {
				t.Errorf("invalid UTC year did not surface the exact serialization failure: %v", operationError)
			}
			f.app = app
			app = f.checkReopened(t, false)
			if all, err := app.Candidates(secondActor, secondProject); err != nil || !reflect.DeepEqual(all, []Candidate{f.candidate}) {
				t.Errorf("serialization failure changed the original candidate collection: %v", err)
			}
			if reports, err := app.Reports(secondActor, secondProject, f.candidate.ID); err != nil || !reflect.DeepEqual(reports, []Report{f.report}) {
				t.Errorf("serialization failure changed the original report collection: %v", err)
			}
			if _, raw, err := app.ReportHTML(secondActor, secondProject, "RPT-2"); !errors.Is(err, ErrNotFound) || len(raw) != 0 {
				t.Errorf("failed serialization left a partly published report: %d bytes, %v", len(raw), err)
			}
			switch action {
			case "preview":
				f.app = app
				f.preview(t)
			case "candidate":
				made, reused, err := app.CreateCandidate(secondActor, secondProject, input)
				if err != nil || reused || made.ID == f.candidate.ID {
					t.Errorf("valid clock did not publish a distinct candidate: %v", err)
				}
			case "report":
				next, err := app.Freeze(secondActor, secondProject, f.candidate.ID)
				if err != nil || next.ID != "RPT-2" || next.Status != Sufficient {
					t.Fatalf("valid clock did not publish the next report: %q, %v", next.ID, err)
				}
				if _, html, err := app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || !bytes.Equal(html, f.html) {
					t.Errorf("report recovery changed historical HTML: %v", err)
				}
			}
		})
	}
}
