package board_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"release-evidence-board/internal/board"
)

func TestCR1YearOneResultControlsFreshness(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	roster := vectorBaseline()
	roster.Behaviors[0].Policy.MaxAgeHours = 1
	roster.Behaviors[0].Tests[0].PlannedAttempts = 2
	c := fixtureCandidate(t, app, roster)
	fresh := "2026-11-17T12:00:00Z"
	yearOne := "0001-01-01T00:00:00Z"
	importBytes(t, app, c.ID, csvBytes(t,
		runOnly(c, "RUN0", yearOne),
		resultRow(c, "T1", "1", "RUN1", "RES1", yearOne, fresh, "PASS"),
		resultRow(c, "T1", "2", "RUN1", "RES2", fresh, fresh, "PASS")))
	result, err := app.Assess(supervisor, project, c.ID)
	if err != nil || result.Status != board.Blocked || result.Behaviors[0].Status != board.Stale ||
		board.FormatUTC(result.EarliestExpiry) != "0001-01-01T01:00:00Z" {
		t.Fatalf("assessment across full RFC3339 years = %s/%s expiry %s, %v", result.Status, result.Behaviors[0].Status, result.EarliestExpiry, err)
	}
	report, err := app.Freeze(supervisor, project, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, html, err := app.ReportHTML(supervisor, project, report.ID)
	if err != nil || !bytes.Contains(html, []byte("Historical assessment · BLOCKED")) ||
		!bytes.Contains(html, []byte("Producer finish <time>0001-01-01T00:00:00Z</time>")) ||
		!bytes.Contains(html, []byte("result finish 0001-01-01T00:00:00Z")) {
		t.Fatal("offline report lost valid early timestamps or the blocked status")
	}
}

func TestCR2DamagedEvidenceCannotProduceSufficient(t *testing.T) {
	for _, damage := range []string{"missing_run_index", "changed_outcome", "missing_failure_row", "changed_approved_roster"} {
		t.Run(damage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history.db")
			options := board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return fixtureTime }}
			app, err := board.Open(path, options)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if app != nil {
					if err := app.Close(); err != nil {
						t.Error(err)
					}
				}
			}()
			if err := app.Bootstrap(supervisor, testPassword); err != nil {
				t.Fatal(err)
			}
			roster := vectorBaseline()
			roster.Behaviors[0].Tests = append(roster.Behaviors[0].Tests,
				board.TestPlan{ID: "T2", ExpectedOutcome: "Unmasked output", PlannedAttempts: 1})
			c := fixtureCandidate(t, app, roster)
			oldAt := "2026-11-17T11:00:00Z"
			newAt := "2026-11-17T11:30:00Z"
			importBytes(t, app, c.ID, csvBytes(t,
				resultRow(c, "T1", "1", "RUN1", "RES1", oldAt, oldAt, "PASS"),
				resultRow(c, "T2", "1", "RUN1", "RES2", oldAt, oldAt, "FAIL"),
				resultRow(c, "T1", "1", "RUN2", "RES3", newAt, newAt, "PASS"),
				resultRow(c, "T2", "1", "RUN2", "RES4", newAt, newAt, "PASS")))
			baseline, err := app.Assess(supervisor, project, c.ID)
			if err != nil || baseline.Behaviors[0].Status != board.Fail {
				t.Fatalf("bad fixture, original failure absent: %v", err)
			}
			if err := app.Close(); err != nil {
				t.Fatal(err)
			}
			app = nil
			damageEvidence(t, path, c, damage)
			app, err = board.Open(path, options)
			if err != nil {
				t.Fatal(err)
			}
			if assessment, err := app.Assess(supervisor, project, c.ID); !errors.Is(err, board.ErrIntegrity) {
				t.Errorf("live check returned %q, %v from damaged retained records", assessment.Status, err)
			}
			if report, err := app.Freeze(supervisor, project, c.ID); !errors.Is(err, board.ErrIntegrity) {
				t.Errorf("freeze returned %q, %v from damaged retained records", report.Status, err)
			}
			if err := app.Revalidate(supervisor, project); !errors.Is(err, board.ErrIntegrity) {
				t.Errorf("review revalidated damaged retained records: %v", err)
			}
			if err := app.Close(); err != nil {
				t.Fatal(err)
			}
			app = nil
			restored := filepath.Join(t.TempDir(), "restored.db")
			result, err := board.RestoreBackup(path, restored, options)
			if err != nil || result.IntegrityOK {
				t.Errorf("restore reported corrupt evidence as verified: %#v, %v", result, err)
			}
		})
	}
}

func damageEvidence(t *testing.T, path string, candidate board.Candidate, damage string) {
	t.Helper()
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	err = db.Update(func(tx *bolt.Tx) error {
		switch damage {
		case "missing_run_index":
			return tx.Bucket([]byte("candidate_runs")).Delete([]byte(candidate.ID + "\x00RUN1"))
		case "changed_approved_roster":
			data := tx.Bucket([]byte("baselines"))
			key := []byte(candidate.BaselineDigest)
			var approved board.ApprovedBaseline
			if err := json.Unmarshal(data.Get(key), &approved); err != nil {
				return err
			}
			approved.Baseline.Behaviors[0].Required = false
			changed, err := json.Marshal(approved)
			if err != nil {
				return err
			}
			return data.Put(key, changed)
		default:
			data := tx.Bucket([]byte("runs"))
			key := []byte("RUN1")
			var run board.Run
			if err := json.Unmarshal(data.Get(key), &run); err != nil {
				return err
			}
			if damage == "changed_outcome" {
				run.Rows[1].Fields[16] = "PASS"
			} else {
				run.Rows = run.Rows[:1]
			}
			changed, err := json.Marshal(run)
			if err != nil {
				return err
			}
			return data.Put(key, changed)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCR3ExactReplayLeavesDatabaseBytesUntouched(t *testing.T) {
	app, path := openBoard(t, func() time.Time { return fixtureTime })
	c := fixtureCandidate(t, app, vectorBaseline())
	at := board.FormatUTC(fixtureTime)
	raw := csvBytes(t, resultRow(c, "T1", "1", "RUN1", "RES1", at, at, "PASS"))
	preview, err := app.PreviewImport(supervisor, project, c.ID, raw)
	if err != nil || preview.Token == "" {
		t.Fatalf("preview = %#v, %v", preview, err)
	}
	first, err := app.CommitImport(supervisor, project, preview.Token, raw)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := app.CommitImport(supervisor, project, preview.Token, raw)
	if err != nil || retry.ID != first.ID {
		t.Fatalf("retry = %#v, %v", retry, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("exact committed retry modified database bytes")
	}
}
