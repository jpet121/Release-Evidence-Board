package board_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"release-evidence-board/internal/board"
)

func TestTicket80OpenKeepsInitializationFailureFatalAndValidationHeld(t *testing.T) {
	for _, phase := range []string{"initialization", "validation"} {
		t.Run(phase, func(t *testing.T) {
			settings := board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return fixtureTime }}
			app, path := openBoard(t, settings.Clock)
			candidate := fixtureCandidate(t, app, vectorBaseline())
			stamp := board.FormatUTC(fixtureTime)
			source := csvBytes(t, resultRow(candidate, "T1", "1", "PRIOR", "PRIOR-RESULT", stamp, stamp, "PASS"))
			receipt := importBytes(t, app, candidate.ID, source)
			prior, err := app.Freeze(supervisor, project, candidate.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, html, err := app.ReportHTML(supervisor, project, prior.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := app.Close(); err != nil {
				t.Fatal(err)
			}
			edit := func(repair bool) {
				t.Helper()
				db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
				if err != nil {
					t.Fatalf("store handle was not released: %v", err)
				}
				err = db.Update(func(tx *bolt.Tx) error {
					if phase == "initialization" {
						if repair {
							_, err := tx.CreateBucket([]byte("preview_csv"))
							return err
						}
						return tx.DeleteBucket([]byte("preview_csv"))
					}
					meta := tx.Bucket([]byte("meta"))
					if repair {
						return meta.Delete([]byte("report_failures"))
					}
					return meta.Put([]byte("report_failures"), []byte("malformed-counter"))
				})
				if err := errors.Join(err, db.Close()); err != nil {
					t.Fatal(err)
				}
			}
			edit(false)
			opened, err := board.Open(path, settings)
			owned := opened
			t.Cleanup(func() {
				if owned != nil {
					if err := owned.Close(); err != nil {
						t.Error(err)
					}
				}
			})
			if phase == "initialization" {
				if opened != nil {
					if closeErr := opened.Close(); closeErr != nil {
						t.Error(closeErr)
					} else {
						owned = nil
					}
				}
				if opened != nil || !errors.Is(err, board.ErrIntegrity) {
					t.Fatalf("initialization damage was admitted as a readable store: opened=%t, %v", opened != nil, err)
				}
			} else {
				if err != nil || opened == nil {
					t.Fatalf("validation damage lost readable recovery access: %v", err)
				}
				if held, err := opened.AssessmentSuspended(supervisor, project); err != nil || !held {
					t.Fatalf("startup failed to hold immediately, before any assessment: held=%t, %v", held, err)
				}
				if account, err := opened.Authenticate(supervisor, testPassword); err != nil || account.ID != supervisor {
					t.Fatalf("held store lost authentication for recovery: %+v, %v", account, err)
				}
				if _, err := opened.Assess(supervisor, project, candidate.ID); !errors.Is(err, board.ErrIntegrity) {
					t.Fatalf("damaged startup allowed assessment: %v", err)
				}
				if err := opened.Close(); err != nil {
					t.Fatal(err)
				}
				owned = nil
			}
			edit(true)
			recovered, err := board.Open(path, settings)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := recovered.Close(); err != nil {
					t.Error(err)
				}
			})
			if held, err := recovered.AssessmentSuspended(supervisor, project); err != nil || held != (phase == "validation") {
				t.Fatalf("persisted hold did not respect startup phase: held=%t, %v", held, err)
			}
			if err := recovered.Revalidate(supervisor, project); err != nil {
				t.Fatal(err)
			}
			if assessment, err := recovered.Assess(supervisor, project, candidate.ID); err != nil || assessment.Status != board.Sufficient || assessment.LedgerHighWater != 1 {
				t.Fatalf("repair/revalidation lost prior assessment: %+v, %v", assessment, err)
			}
			if raw, err := recovered.SourceCSV(supervisor, project, receipt.ID); err != nil || !bytes.Equal(raw, source) {
				t.Fatalf("startup failure changed original evidence: %v", err)
			}
			if report, raw, err := recovered.ReportHTML(supervisor, project, prior.ID); err != nil || report != prior || !bytes.Equal(raw, html) {
				t.Fatalf("startup failure changed historical report: %v", err)
			}
		})
	}
}
