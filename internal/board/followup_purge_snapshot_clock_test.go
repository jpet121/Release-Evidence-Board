package board

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupPurgeRechecksHistoricalBudgetAtItsOwnClock(t *testing.T) {
	for _, operation := range []string{"preview", "confirm"} {
		t.Run(operation, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			at := f.at.Add(181 * 24 * time.Hour)
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				for _, name := range []string{aiHistoryBucket, aiHistoryBinding, aiBudgetBucket, aiBudgetBinding} {
					if _, err := tx.CreateBucket([]byte(name)); err != nil {
						return err
					}
				}
				if err := tx.Bucket([]byte(metaBucket)).Put([]byte(aiSnapshotKey), []byte(aiSnapshotVersion)); err != nil {
					return err
				}
				fact := aiBudgetFact{RequestedAt: FormatUTC(at), State: "billed", Amount: 100}
				return putBoundJSON(tx, aiBudgetBucket, aiBudgetBinding, "synthetic-budget-fact", fact)
			}); err != nil {
				t.Fatal(err)
			}
			path := f.app.db.Path()
			if err := f.app.Close(); err != nil {
				t.Fatal(err)
			}
			var scheduled, observed []time.Time
			app, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time {
				if len(scheduled) == 0 {
					return at
				}
				now := scheduled[0]
				scheduled = scheduled[1:]
				observed = append(observed, now)
				return now
			}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := app.Close(); err != nil {
					t.Error(err)
				}
			})
			if health, err := app.Health(secondActor, secondProject); err != nil || health.HistoricalAI == nil || health.HistoricalAI.BilledToday != 100 {
				t.Fatalf("valid synthetic history was not recognized at its own time: %v", err)
			}
			scope := []string{f.candidate.ID}
			before, err := app.PreviewPurge(secondActor, secondProject, scope)
			if err != nil || len(before.Ineligible) != 0 || len(before.ReceiptIDs) != 1 || len(before.ReportIDs) != 1 {
				t.Fatalf("healthy clock did not produce the exact eligible purge: %v", err)
			}
			earlier := at.Add(-time.Microsecond)
			scheduled = []time.Time{at, earlier}
			var plan PurgePlan
			if operation == "preview" {
				plan, err = app.PreviewPurge(secondActor, secondProject, scope)
			} else {
				plan, err = app.Purge(secondActor, secondProject, scope, before.Confirmation)
			}
			if len(observed) != 2 || !observed[0].Equal(at) || !observed[1].Equal(earlier) {
				t.Fatalf("expected verification and planning to observe distinct clocks: %v", observed)
			}
			if !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "historical AI accounting fact is invalid") || plan.Confirmation != "" {
				t.Errorf("purge accepted historical budget outside its planning clock: %v", err)
			}
			if csv, err := app.SourceCSV(secondActor, secondProject, f.receipt.ID); err != nil || !bytes.Equal(csv, f.source) {
				t.Errorf("failed clock admission changed independent original evidence: %v", err)
			}
			if report, raw, err := app.ReportHTML(secondActor, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(raw, f.html) {
				t.Errorf("failed clock admission changed independent frozen HTML: %v", err)
			}
			if err := app.Revalidate(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			if after, err := app.PreviewPurge(secondActor, secondProject, scope); err != nil || !reflect.DeepEqual(after, before) {
				t.Errorf("restored valid clock did not recover the same purge confirmation: %v", err)
			}
		})
	}
}
