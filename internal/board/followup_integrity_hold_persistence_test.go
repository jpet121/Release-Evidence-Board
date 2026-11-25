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

func TestFollowupIntegrityHoldSurvivesPersistenceFailure(t *testing.T) {
	for _, fault := range []string{"audit_collision", "nested_hold_key", "durable_hold"} {
		t.Run(fault, func(t *testing.T) {
			app, baseline, at := reviewBoard(t)
			const viewer = "hold-viewer"
			if err := app.SetAccount(secondActor, viewer, "synthetic-viewer-password", []Role{Viewer}); err != nil {
				t.Fatal(err)
			}
			damaged := reviewCandidate(t, app, baseline, "a")
			damagedReceipt, _, original := reviewCommit(t, app, damaged, reviewRows(damaged, "a", "DAMAGED", Pass, at))
			healthy := reviewCandidate(t, app, baseline, "b")
			reviewCommit(t, app, healthy, reviewRows(healthy, "b", "HEALTHY", Pass, at))
			frozen, err := app.Freeze(secondActor, secondProject, healthy.ID)
			if err != nil || frozen.Status != Sufficient {
				t.Fatalf("fixture could not freeze sufficient evidence for the independent candidate: %v", err)
			}
			metadata, html, err := app.ReportHTML(viewer, secondProject, frozen.ID)
			if err != nil || len(html) == 0 {
				t.Fatalf("fixture could not read frozen evidence: %v", err)
			}
			auditBefore, err := app.Audit(secondActor, secondProject)
			if err != nil || len(auditBefore) == 0 {
				t.Fatalf("fixture lacks retained audit evidence: %v", err)
			}

			// Prepare storage faults in the isolated real store; all outcomes
			// below are observed through public Board operations.
			holdKey := []byte("assessment_hold")
			var auditSequence uint64
			if err := app.db.Update(func(tx *bolt.Tx) error {
				if err := tx.Bucket([]byte(csvBucket)).Put([]byte(damagedReceipt.Hash), []byte("corrupted source")); err != nil {
					return err
				}
				switch fault {
				case "audit_collision":
					audit := tx.Bucket([]byte(auditBucket))
					auditSequence = audit.Sequence()
					if auditSequence == 0 {
						return errors.New("fixture has no sequence to collide with")
					}
					return audit.SetSequence(auditSequence - 1)
				case "nested_hold_key":
					_, err := tx.Bucket([]byte(metaBucket)).CreateBucket(holdKey)
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			raw, err := app.SourceCSV(secondActor, secondProject, damagedReceipt.ID)
			if len(raw) != 0 || !errors.Is(err, ErrIntegrity) {
				t.Fatalf("corrupt source was readable: %d bytes, %v", len(raw), err)
			}
			switch fault {
			case "audit_collision":
				if !strings.Contains(err.Error(), "audit event sequence already exists") {
					t.Errorf("audit collision was not preserved: %v", err)
				}
			case "nested_hold_key":
				if !errors.Is(err, bolt.ErrIncompatibleValue) {
					t.Errorf("nested hold write failure was not preserved: %v", err)
				}
			}
			after, err := app.Audit(secondActor, secondProject)
			if fault == "durable_hold" {
				if err != nil || len(after) != len(auditBefore)+1 ||
					!reflect.DeepEqual(after[:len(auditBefore)], auditBefore) ||
					after[len(auditBefore)].Action != "assessment.integrity_hold" ||
					after[len(auditBefore)].Actor != secondActor || after[len(auditBefore)].Outcome != "failure" {
					t.Errorf("durable integrity hold audit differs: %v", err)
				}
			} else if err != nil || !reflect.DeepEqual(after, auditBefore) {
				t.Errorf("failed hold transaction changed the retained audit: %v", err)
			}

			// Repair only the injected faults. A live hold must keep blocking
			// decisions until revalidation, even after the write can succeed.
			if err := app.db.Update(func(tx *bolt.Tx) error {
				if err := tx.Bucket([]byte(csvBucket)).Put([]byte(damagedReceipt.Hash), original); err != nil {
					return err
				}
				switch fault {
				case "audit_collision":
					return tx.Bucket([]byte(auditBucket)).SetSequence(auditSequence)
				case "nested_hold_key":
					return tx.Bucket([]byte(metaBucket)).DeleteBucket(holdKey)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if held, err := app.AssessmentSuspended(viewer, secondProject); err != nil || !held {
				t.Errorf("hold was lost after failed persistence or durable write: held=%t, %v", held, err)
			}
			if assessment, err := app.Assess(viewer, secondProject, healthy.ID); !errors.Is(err, ErrIntegrity) || assessment.Status != "" {
				t.Errorf("healthy candidate assessment escaped hold: status=%q, %v", assessment.Status, err)
			}
			if report, err := app.Freeze(secondActor, secondProject, healthy.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
				t.Errorf("healthy candidate freeze escaped hold: report=%q, %v", report.ID, err)
			}
			saved, preserved, err := app.ReportHTML(viewer, secondProject, frozen.ID)
			if err != nil || !reflect.DeepEqual(saved, metadata) || !bytes.Equal(preserved, html) {
				t.Errorf("hold changed verified historical report metadata or bytes: %v", err)
			}

			file := app.db.Path()
			if err := app.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(file, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return at }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := reopened.Close(); err != nil {
					t.Error(err)
				}
			})
			wantDurable := fault == "durable_hold"
			if held, err := reopened.AssessmentSuspended(viewer, secondProject); err != nil || held != wantDurable {
				t.Fatalf("reopened hold=%t, want %t after the observed persistence outcome: %v", held, wantDurable, err)
			}
			if wantDurable {
				log, err := reopened.Audit(secondActor, secondProject)
				if err != nil || len(log) < len(after) || !reflect.DeepEqual(log[:len(after)], after) {
					t.Fatalf("reopening changed exact retained hold audit: %v", err)
				}
				if err := reopened.Revalidate(viewer, secondProject); !errors.Is(err, ErrForbidden) {
					t.Errorf("viewer revalidation got %v, want ErrForbidden", err)
				}
				if held, err := reopened.AssessmentSuspended(viewer, secondProject); err != nil || !held {
					t.Errorf("viewer revalidation changed durable hold: held=%t, %v", held, err)
				}
				if err := reopened.Revalidate(secondActor, secondProject); err != nil {
					t.Fatalf("reviewer could not revalidate repaired evidence: %v", err)
				}
			}
			if assessment, err := reopened.Assess(viewer, secondProject, healthy.ID); err != nil || assessment.Status != Sufficient {
				t.Errorf("healthy candidate did not return to sufficient after the observed recovery: status=%q, %v", assessment.Status, err)
			}
			saved, preserved, err = reopened.ReportHTML(viewer, secondProject, frozen.ID)
			if err != nil || !reflect.DeepEqual(saved, metadata) || !bytes.Equal(preserved, html) {
				t.Errorf("recovery changed verified historical report metadata or bytes: %v", err)
			}
		})
	}
}
