package browser_test

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupOperatorHealthShowsSafeLiveCountersAndAIState(t *testing.T) {
	fx := newAuthorityFixture(t)
	candidate, _, _, err := fx.app.Candidate("reviewer", "pilot", fx.candidateID)
	if err != nil {
		t.Fatal(err)
	}
	var captured bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&captured)
	t.Cleanup(func() { log.SetOutput(previous) })
	log.Print("synthetic log capture control")
	const rawPayload = "SYNTHETIC-RAW-PAYLOAD-MARKER"
	const token = "SYNTHETIC-UNBOUND-TOKEN-MARKER"
	const excerpt = "SYNTHETIC-FAILURE-EXCERPT-MARKER"
	stamp := board.FormatUTC(time.Now().UTC().Add(-time.Minute))
	row := followupBrowserResult(candidate, "R1", "BEH1", "P1", "T1", "LOG-RUN", "LOG-RESULT", stamp, "FAIL")
	row[11], row[18] = rawPayload, excerpt
	basePath := "/p/pilot/candidates/" + candidate.ID
	preview := fx.send(t, fx.reviewer, basePath+"/preview", nil, followupBrowserCSV(t, row))
	if preview.status != http.StatusUnprocessableEntity {
		t.Fatalf("real invalid CSV preview returned HTTP %d", preview.status)
	}
	unbound := fx.send(t, fx.reviewer, basePath+"/commit", url.Values{"preview": {token}}, nil)
	if unbound.status != http.StatusNotFound {
		t.Fatalf("unbound material confirmation returned HTTP %d", unbound.status)
	}
	if err := fx.app.SuspendAssessments("reviewer", "pilot"); err != nil {
		t.Fatal(err)
	}
	held := fx.send(t, fx.reviewer, basePath+"/reports", nil, nil)
	if held.status != http.StatusServiceUnavailable {
		t.Fatalf("held report creation returned HTTP %d", held.status)
	}
	if err := fx.app.Revalidate("reviewer", "pilot"); err != nil {
		t.Fatal(err)
	}
	backup, err := fx.app.Backup("reviewer", "pilot", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if healthy := fx.app.Check(); !healthy.DatabaseOK || healthy.ConsecutiveFailures != 0 || healthy.BackupOverdue {
		t.Fatalf("synthetic denied actions degraded healthy storage availability: %+v", healthy)
	}
	html, status := response(t, fx.operator.client.Get, fx.visit+"/p/pilot/health")
	if status != http.StatusOK {
		t.Fatalf("healthy operator view returned HTTP %d", status)
	}
	for _, expected := range []string{
		"<dt>Database</dt><dd>Available</dd>",
		"<dt>Last successful backup</dt><dd>" + board.FormatUTC(backup.CreatedAt) + "</dd>",
		"<dt>Import rejections</dt><dd>1</dd>",
		"<dt>Failed snapshots</dt><dd>1</dd>",
		"<dt>Optional AI</dt><dd>Disabled · no provider configured</dd>",
	} {
		if !strings.Contains(html, expected) {
			t.Fatalf("healthy operator view omitted %q", expected)
		}
	}
	if !strings.Contains(captured.String(), "synthetic log capture control") {
		t.Fatal("ordinary logger was not attached to evidence capture")
	}
	for _, marker := range []string{rawPayload, token, excerpt} {
		if strings.Contains(captured.String(), marker) || strings.Contains(html, marker) {
			t.Fatalf("ordinary logs or health leaked synthetic marker %q", marker)
		}
	}
	recordCorroboratedBrowserObservation(t)
}

func TestFollowupDirectAuditMutationAttemptsCannotChangeHistory(t *testing.T) {
	fx := newAuthorityFixture(t)
	before := fx.audit(t)
	if len(before) == 0 {
		t.Fatal("fixture has no attributed prior events")
	}
	path := fmt.Sprintf("%s/p/pilot/audit/%d", fx.visit, before[0].Sequence)
	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			form := url.Values{"_csrf": {fx.reviewer.csrf}, "outcome": {"replaced"}}
			request, err := http.NewRequest(method, path, strings.NewReader(form.Encode()))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			_, status := response(t, func(string) (*http.Response, error) { return fx.reviewer.client.Do(request) }, path)
			if status != http.StatusNotFound && status != http.StatusMethodNotAllowed && status != http.StatusForbidden {
				t.Fatalf("direct alteration of retained audit returned HTTP %d", status)
			}
			if after := fx.audit(t); !reflect.DeepEqual(after, before) {
				t.Fatal("attempted audit mutation changed the retained event history")
			}
			recordCorroboratedBrowserObservation(t)
		})
	}
}
