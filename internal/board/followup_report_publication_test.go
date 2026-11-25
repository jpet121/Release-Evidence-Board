package board

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestFollowupFreezeDoesNotOverwriteReport(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	_, _, baseline, err := f.app.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	other := reviewCandidate(t, f.app, baseline, "b")
	if f.report.ID != "RPT-1" {
		t.Fatalf("fixture first report ID = %q, want RPT-1", f.report.ID)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		// Rewind only the allocator in the disposable store. Retain the
		// original report and all its verified evidence.
		return tx.Bucket([]byte(reportBucket)).SetSequence(0)
	}); err != nil {
		t.Fatal(err)
	}
	attempt, err := f.app.Freeze(secondActor, secondProject, other.ID)
	if !errors.Is(err, ErrIntegrity) || attempt.ID != "" {
		t.Errorf("duplicate report identifier was accepted: report=%q, error=%v", attempt.ID, err)
	}
	metadata, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID)
	if err != nil || !reflect.DeepEqual(metadata, f.report) || !bytes.Equal(html, f.html) {
		t.Errorf("duplicate report identifier changed the existing frozen export: candidate=%q, status=%q, error=%v", metadata.CandidateID, metadata.Status, err)
	}
	reports, err := f.app.Reports(secondActor, secondProject, other.ID)
	if err != nil || len(reports) != 0 {
		t.Errorf("duplicate report attempt was published on the new candidate: %v", err)
	}
	if held, err := f.app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
		t.Errorf("allocation integrity fault did not suspend future assessment: held=%t, %v", held, err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
		t.Errorf("reviewer cleared a hold with an unrepaired report allocator: %v", err)
	}
	if held, err := f.app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
		t.Errorf("unrepaired allocator no longer holds the board: held=%t, %v", held, err)
	}
	if a, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || a.Status != "" {
		t.Errorf("unrepaired report allocator allowed a new assessment: status=%q, %v", a.Status, err)
	}
	if report, err := f.app.Freeze(secondActor, secondProject, other.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
		t.Errorf("unrepaired report allocator allowed a new export: report=%q, %v", report.ID, err)
	}
	metadata, html, err = f.app.ReportHTML(secondActor, secondProject, f.report.ID)
	if err != nil || metadata != f.report || !bytes.Equal(html, f.html) {
		t.Errorf("revalidation changed the verified historical report: %v", err)
	}

	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(reportBucket)).SetSequence(1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("reviewer could not revalidate a repaired report allocator: %v", err)
	}
	if a, err := f.app.Assess(secondActor, secondProject, f.candidate.ID); err != nil || a.Status != Sufficient {
		t.Errorf("intact evidence did not regain a sufficient assessment after repair: status=%q, %v", a.Status, err)
	}
	if report, err := f.app.Freeze(secondActor, secondProject, other.ID); err != nil || report.ID != "RPT-2" {
		t.Errorf("genuine allocator repair did not allow the next distinct report: %q, %v", report.ID, err)
	}
}

func TestFollowupFreezeCannotWrapReportAllocator(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		// Retention can leave the allocator ahead of all retained reports.
		return tx.Bucket([]byte(reportBucket)).SetSequence(math.MaxUint64 - 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); err != nil {
		t.Fatalf("healthy allocator with unused retained ID gaps was rejected: %v", err)
	}
	const finalID = "RPT-18446744073709551615"
	final, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID)
	if err != nil || final.ID != finalID || final.Status != Sufficient {
		t.Fatalf("failed to publish the final canonical report: %q, %q, %v", final.ID, final.Status, err)
	}
	final, finalHTML, err := f.app.ReportHTML(secondActor, secondProject, finalID)
	if err != nil || len(finalHTML) == 0 {
		t.Fatalf("cannot read final verified report: %v", err)
	}
	checkHistory := func(app *Board, scope string) {
		t.Helper()
		for _, record := range []struct {
			report Report
			html   []byte
		}{{f.report, f.html}, {final, finalHTML}} {
			got, html, err := app.ReportHTML(secondActor, secondProject, record.report.ID)
			if err != nil || got != record.report || !bytes.Equal(html, record.html) {
				t.Errorf("%s changed verified report %s: %v", scope, record.report.ID, err)
			}
		}
	}
	checkTerminal := func(app *Board, scope string) {
		t.Helper()
		if err := app.Revalidate(secondActor, secondProject); err != nil {
			t.Errorf("%s reviewer could not verify the genuine terminal report: %v", scope, err)
		}
		if a, err := app.Assess(secondActor, secondProject, f.candidate.ID); err != nil || a.Status != Sufficient || a.LedgerHighWater != 1 {
			t.Errorf("%s terminal allocator prevented an honest assessment: %q, %v", scope, a.Status, err)
		}
		for range 2 {
			attempt, err := app.Freeze(secondActor, secondProject, f.candidate.ID)
			if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrIntegrity) || attempt.ID != "" {
				t.Errorf("%s terminal publication did not refuse as a capacity limit: report=%q, %v", scope, attempt.ID, err)
			}
		}
		if held, err := app.AssessmentSuspended(secondActor, secondProject); err != nil || held {
			t.Errorf("%s valid terminal allocator was treated as uncertainty: held=%t, %v", scope, held, err)
		}
		if _, raw, err := app.ReportHTML(secondActor, secondProject, "RPT-0"); !errors.Is(err, ErrNotFound) || len(raw) != 0 {
			t.Errorf("%s wrapped report identifier became readable: %d bytes, %v", scope, len(raw), err)
		}
		checkHistory(app, scope)
	}
	backup, backupErr := f.app.Backup(secondActor, secondProject, t.TempDir())
	if backupErr != nil {
		t.Errorf("backup refused the genuine terminal report: %v", backupErr)
	}
	checkTerminal(f.app, "live")
	file := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	opts := Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return f.at }}
	reopen := func(path string) *Board {
		t.Helper()
		app, err := Open(path, opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := app.Close(); err != nil {
				t.Error(err)
			}
		})
		return app
	}
	checkTerminal(reopen(file), "reopened")
	if backupErr == nil {
		destination := filepath.Join(t.TempDir(), "terminal-restore.db")
		restored, err := RestoreBackup(backup.Path, destination, opts)
		if err != nil || !restored.IntegrityOK {
			t.Fatalf("genuine terminal backup could not be restored: %v", err)
		}
		app := reopen(destination)
		if held, err := app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
			t.Errorf("terminal restore bypassed its ordinary reviewer hold: %v", err)
		}
		checkTerminal(app, "restored")
	}
}

func TestFollowupRevalidateRejectsNoncanonicalFrozenReportID(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	if err := f.app.SuspendAssessments(secondActor, secondProject); err != nil {
		t.Fatal(err)
	}
	alias := f.report
	alias.ID = "RPT-01"
	metadata, err := json.Marshal(alias)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		// The companion records all agree with the alias. The only semantic
		// fault is a second spelling for one canonical report number.
		if err := tx.Bucket([]byte(reportBucket)).Put([]byte(alias.ID), metadata); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(htmlBucket)).Put([]byte(alias.ID), f.html); err != nil {
			return err
		}
		return putJSON(tx, reportMetaBucket, alias.ID, frozenReportBinding{MetadataSHA256: digest(metadata), HTMLSHA256: alias.SHA256})
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Revalidate(secondActor, secondProject); !errors.Is(err, ErrIntegrity) {
		t.Errorf("reviewer accepted a second spelling of a frozen report number: %v", err)
	}
	metadataRecord, html, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID)
	if err != nil || metadataRecord != f.report || !bytes.Equal(html, f.html) {
		t.Errorf("alias validation changed the verified original report: %v", err)
	}
}

func TestFollowupFreezeTransactionPublishesAllOrNone(t *testing.T) {
	for _, fault := range []struct {
		name, bucket string
		nested       bool
	}{
		{"html", htmlBucket, true},
		{"metadata", reportBucket, true},
		{"binding", reportMetaBucket, true},
		{"orphan_html", htmlBucket, false},
		{"orphan_metadata", reportBucket, false},
		{"orphan_binding", reportMetaBucket, false},
	} {
		t.Run(fault.name, func(t *testing.T) {
			f := newIntakeCustodyFixture(t)
			const nextID = "RPT-2"
			if f.report.ID != "RPT-1" {
				t.Fatalf("fixture first report ID = %q, want RPT-1", f.report.ID)
			}
			before, err := f.app.Audit(secondActor, secondProject)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				bucket := tx.Bucket([]byte(fault.bucket))
				if fault.nested {
					_, err := bucket.CreateBucket([]byte(nextID))
					return err
				}
				return bucket.Put([]byte(nextID), []byte("occupied damaged report component"))
			}); err != nil {
				t.Fatal(err)
			}
			if report, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
				t.Fatalf("occupied report component was not rejected as an integrity fault: report=%q, %v", report.ID, err)
			}
			if err := f.app.db.Update(func(tx *bolt.Tx) error {
				bucket := tx.Bucket([]byte(fault.bucket))
				if fault.nested {
					return bucket.DeleteBucket([]byte(nextID))
				}
				return bucket.Delete([]byte(nextID))
			}); err != nil {
				t.Fatal(err)
			}

			file := f.app.db.Path()
			if err := f.app.Close(); err != nil {
				t.Fatal(err)
			}
			app, err := Open(file, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time { return f.at }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := app.Close(); err != nil {
					t.Error(err)
				}
			})
			reports, err := app.Reports(secondActor, secondProject, f.candidate.ID)
			if err != nil || !reflect.DeepEqual(reports, []Report{f.report}) {
				t.Errorf("failed snapshot changed the reopened report inventory: %v", err)
			}
			if _, raw, err := app.ReportHTML(secondActor, secondProject, nextID); !errors.Is(err, ErrNotFound) || len(raw) != 0 {
				t.Errorf("failed snapshot exposed a partially published export: %d bytes, %v", len(raw), err)
			}
			report, raw, err := app.ReportHTML(secondActor, secondProject, f.report.ID)
			if err != nil || !reflect.DeepEqual(report, f.report) || !bytes.Equal(raw, f.html) {
				t.Errorf("failed snapshot changed the prior frozen report: %v", err)
			}
			audit, err := app.Audit(secondActor, secondProject)
			if err != nil || len(audit) != len(before)+2 || !reflect.DeepEqual(audit[:len(before)], before) ||
				audit[len(before)].Actor != secondActor || audit[len(before)].Action != "assessment.integrity_hold" ||
				audit[len(before)].Outcome != "failure" ||
				audit[len(before)+1].Actor != secondActor || audit[len(before)+1].Action != "report.freeze" ||
				audit[len(before)+1].Outcome != "failure" {
				t.Errorf("failed snapshot lacked its exact failure audit: %v", err)
			}
			if held, err := app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
				t.Errorf("occupied report component did not cause a durable hold: held=%t, %v", held, err)
			}
			if err := app.Revalidate(secondActor, secondProject); err != nil {
				t.Fatalf("reviewer could not revalidate after removing the occupied component: %v", err)
			}
			fresh, err := app.Freeze(secondActor, secondProject, f.candidate.ID)
			if err != nil || fresh.ID != nextID || fresh.Status != Sufficient {
				t.Errorf("fresh snapshot did not publish once the fault was removed: %q, %v", fresh.ID, err)
			}
		})
	}
}

func TestFollowupFreezeRefusesMissingPublicationStore(t *testing.T) {
	for _, section := range []string{reportBucket, htmlBucket, reportMetaBucket} {
		t.Run(section, func(t *testing.T) {
			app, baseline, _ := reviewBoard(t)
			candidate := reviewCandidate(t, app, baseline, "a")
			if err := app.db.Update(func(tx *bolt.Tx) error {
				return tx.DeleteBucket([]byte(section))
			}); err != nil {
				t.Fatal(err)
			}
			if report, err := app.Freeze(secondActor, secondProject, candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
				t.Errorf("missing publication store did not refuse a new report: report=%q, %v", report.ID, err)
			}
			if held, err := app.AssessmentSuspended(secondActor, secondProject); err != nil || !held {
				t.Errorf("missing publication store did not hold further assessments: held=%t, %v", held, err)
			}
			if err := app.db.Update(func(tx *bolt.Tx) error {
				_, err := tx.CreateBucket([]byte(section))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := app.Revalidate(secondActor, secondProject); err != nil {
				t.Fatal(err)
			}
			if report, err := app.Freeze(secondActor, secondProject, candidate.ID); err != nil || report.ID != "RPT-1" {
				t.Errorf("first report did not publish after restoring the store: report=%q, %v", report.ID, err)
			}
		})
	}
}

func TestFollowupFreezeCannotPublishAnUnverifiedAssessment(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(candidateRun)).Delete([]byte(f.candidate.ID + "\x00PRIOR"))
	}); err != nil {
		t.Fatal(err)
	}
	if report, err := f.app.Freeze(secondActor, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || report.ID != "" {
		t.Errorf("unverified assessment unexpectedly allowed a frozen report %q: %v", report.ID, err)
	}
	if _, raw, err := f.app.ReportHTML(secondActor, secondProject, "RPT-2"); !errors.Is(err, ErrNotFound) || len(raw) != 0 {
		t.Errorf("unverified assessment exposed a new artifact: %d bytes, %v", len(raw), err)
	}
	report, raw, err := f.app.ReportHTML(secondActor, secondProject, f.report.ID)
	if err != nil || !reflect.DeepEqual(report, f.report) || !bytes.Equal(raw, f.html) {
		t.Errorf("unverified assessment changed a verified historical artifact: %v", err)
	}
}

func TestFollowupHistoricalReportAccessChecksOwnerScope(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	const viewer = "report-viewer"
	if err := f.app.SetAccount(secondActor, viewer, "synthetic-viewer-password", []Role{Viewer}); err != nil {
		t.Fatal(err)
	}
	reports, err := f.app.Reports(viewer, secondProject, f.candidate.ID)
	if err != nil || !slices.Equal(reports, []Report{f.report}) {
		t.Fatalf("viewer cannot list its assigned historical report: %v", err)
	}
	if report, raw, err := f.app.ReportHTML(viewer, secondProject, f.report.ID); err != nil || report != f.report || !bytes.Equal(raw, f.html) {
		t.Fatalf("viewer cannot read exact assigned historical export: %v", err)
	}
	if report, err := f.app.Freeze(viewer, secondProject, f.candidate.ID); !errors.Is(err, ErrForbidden) || report.ID != "" {
		t.Errorf("viewer can publish a new report: %q, %v", report.ID, err)
	}
	for _, denied := range []struct {
		actor, project string
		err            error
	}{
		{"unassigned-account", secondProject, ErrForbidden},
		{viewer, "different-project", ErrNotFound},
	} {
		if reports, err := f.app.Reports(denied.actor, denied.project, f.candidate.ID); !errors.Is(err, denied.err) || len(reports) != 0 {
			t.Errorf("denied historical listing returned evidence: %d records, %v", len(reports), err)
		}
		if report, raw, err := f.app.ReportHTML(denied.actor, denied.project, f.report.ID); !errors.Is(err, denied.err) || report.ID != "" || len(raw) != 0 {
			t.Errorf("denied HTML request returned evidence: %q, %d bytes, %v", report.ID, len(raw), err)
		}
	}
	if reports, err := f.app.Reports(viewer, secondProject, "C-missing-candidate"); !errors.Is(err, ErrNotFound) || len(reports) != 0 {
		t.Errorf("unknown candidate report list did not fail closed: %v", err)
	}
	if err := f.app.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(htmlBucket)).Put([]byte(f.report.ID), []byte("changed report payload"))
	}); err != nil {
		t.Fatal(err)
	}
	if reports, err := f.app.Reports(viewer, secondProject, f.candidate.ID); !errors.Is(err, ErrIntegrity) || len(reports) != 0 {
		t.Errorf("corrupt payload appeared in report listing: %d records, %v", len(reports), err)
	}
}
