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

func TestTicket80LateAuditFailureRollsBackFrozenReportAndRetries(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	path := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	armed, reads := false, 0
	app, err := Open(path, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time {
		if armed {
			reads++
			if reads == 2 {
				// Assessment/rendering use a valid time. Only the subsequent
				// success audit cannot be serialized by encoding/json.
				return time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			}
		}
		return f.at
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	f.app = app
	before, err := app.Audit(secondActor, secondProject)
	if err != nil || len(before) == 0 {
		t.Fatalf("read prior audit: %v", err)
	}
	healthBefore, err := app.Health(secondActor, secondProject)
	if err != nil {
		t.Fatal(err)
	}
	armed = true
	partial, operationError := app.Freeze(secondActor, secondProject, f.candidate.ID)
	armed = false
	var marshalError *json.MarshalerError
	if !errors.As(operationError, &marshalError) || !strings.Contains(marshalError.Error(), "year outside") {
		t.Fatalf("late audit error was not propagated: %v", operationError)
	}
	if reads != 3 || partial.ID != "RPT-2" || partial.SHA256 == "" {
		t.Fatalf("did not reach the success audit and subsequent failure audit: reads=%d report=%+v", reads, partial)
	}
	app = f.checkReopened(t, false)
	reports, err := app.Reports(secondActor, secondProject, f.candidate.ID)
	if err != nil || !reflect.DeepEqual(reports, []Report{f.report}) {
		t.Fatalf("late audit refusal published a report: %+v, %v", reports, err)
	}
	if _, html, err := app.ReportHTML(secondActor, secondProject, "RPT-2"); !errors.Is(err, ErrNotFound) || len(html) != 0 {
		t.Fatalf("late audit refusal left partial report bytes: %d, %v", len(html), err)
	}
	after, err := app.Audit(secondActor, secondProject)
	if err != nil || len(after) != len(before)+1 || !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatalf("late audit refusal changed prior events or published a success: %+v, %v", after, err)
	}
	last := after[len(after)-1]
	if last.Sequence != before[len(before)-1].Sequence+1 || last.Action != "report.freeze" ||
		last.Outcome != "failure" || last.Actor != secondActor || !last.Time.Equal(f.at) || len(last.References) != 0 {
		t.Fatalf("failure audit attribution/sequence changed: %+v", last)
	}
	health, err := app.Health(secondActor, secondProject)
	if err != nil || health.ReportFailures != healthBefore.ReportFailures+1 {
		t.Fatalf("failure counter did not survive restart: %+v, %v", health, err)
	}
	next, err := app.Freeze(secondActor, secondProject, f.candidate.ID)
	if err != nil || next.ID != "RPT-2" || next.Status != Sufficient || next.LedgerHighWater != 1 {
		t.Fatalf("retry lost report allocator/evidence state: %+v, %v", next, err)
	}
	prior, html, err := app.ReportHTML(secondActor, secondProject, f.report.ID)
	if err != nil || prior != f.report || !bytes.Equal(html, f.html) {
		t.Fatalf("retry changed historical report bytes: %v", err)
	}
	f.app = app
	app = f.checkReopened(t, false)
	stored, html, err := app.ReportHTML(secondActor, secondProject, "RPT-2")
	if err != nil || stored != next || !bytes.Contains(html, []byte("RPT-2")) || !bytes.Contains(html, []byte("SUFFICIENT")) {
		t.Fatalf("acknowledged retry was not durably published: %+v, %v", stored, err)
	}
	reports, err = app.Reports(secondActor, secondProject, f.candidate.ID)
	if err != nil || len(reports) != 2 {
		t.Fatalf("retry report collection is incomplete: %+v, %v", reports, err)
	}
	wantReports := map[string]Report{f.report.ID: f.report, next.ID: next}
	for _, report := range reports {
		if want, ok := wantReports[report.ID]; !ok || report != want {
			t.Fatalf("retry changed the frozen report collection: %+v", reports)
		}
		delete(wantReports, report.ID)
	}
	retryAudit, err := app.Audit(secondActor, secondProject)
	if err != nil || len(retryAudit) != len(after)+1 || !reflect.DeepEqual(retryAudit[:len(after)], after) {
		t.Fatalf("retry changed prior audit history: %+v, %v", retryAudit, err)
	}
	success := retryAudit[len(retryAudit)-1]
	if success.Sequence != last.Sequence+1 || success.Action != "report.freeze" || success.Outcome != "success" ||
		success.Actor != secondActor || !success.Time.Equal(f.at) || !reflect.DeepEqual(success.References, []string{f.candidate.ID, "RPT-2"}) {
		t.Fatalf("retry lacks its durable attributed success audit: %+v", success)
	}
}
