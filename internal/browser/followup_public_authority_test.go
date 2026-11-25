package browser_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupForeignReadsNeverExposeRetainedEvidence(t *testing.T) {
	app, visit, id := startTestBrowser(t)
	client, _ := loginSession(t, visit, "reviewer")
	candidate, _, baseline, err := app.Candidate("reviewer", "pilot", id)
	if err != nil {
		t.Fatal(err)
	}
	stamp := board.FormatUTC(time.Now().UTC().Add(-time.Minute))
	raw := followupBrowserCSV(t, followupBrowserResult(candidate, "R1", "BEH1", "P1", "T1", "ISOLATED-RUN", "ISOLATED-RESULT", stamp, "PASS"))
	staged, err := app.PreviewImport("reviewer", "pilot", id, raw)
	if err != nil || staged.Token == "" {
		t.Fatalf("positive read fixture did not stage real evidence: %v", err)
	}
	receipt, err := app.CommitImport("reviewer", "pilot", staged.Token, raw)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := app.Freeze("reviewer", "pilot", id)
	if err != nil {
		t.Fatal(err)
	}
	candidatePath := "/candidates/" + id
	for _, item := range []struct{ id, operation, path, witness string }{
		{"AC-001-03.P003", "baseline summary", "/baselines", baseline.Descriptor.Digest},
		{"AC-001-03.P005", "baseline descriptor download", "/baselines/" + baseline.Descriptor.Digest, baseline.Descriptor.Text},
		{"AC-001-03.P007", "evidence board", candidatePath + "?status=PASS", "BEH1"},
		{"AC-001-03.P008", "behavior detail", candidatePath + "/behaviors/BEH1", "ISOLATED-RUN"},
		{"AC-001-03.P009", "run history", candidatePath + "/behaviors?id=BEH1&refresh=off", "ISOLATED-RESULT"},
		{"AC-001-03.P010", "retained source row", candidatePath + "/behaviors/BEH1?refresh=off", "ISOLATED-RESULT · PASS · CSV row"},
		{"AC-001-03.P012", "report list", candidatePath + "/reports", frozen.ID},
		{"AC-001-03.P014", "operator health", "/health", "health"},
	} {
		t.Run(item.id, func(t *testing.T) {
			good, goodStatus := response(t, client.Get, visit+"/p/pilot"+item.path)
			if goodStatus != http.StatusOK || !strings.Contains(good, item.witness) {
				t.Fatalf("same-project positive %s was not a real protected read: HTTP %d", item.operation, goodStatus)
			}
			foreign, status := response(t, client.Get, visit+"/p/foreign"+item.path)
			if status != http.StatusNotFound || !strings.Contains(foreign, "The requested record is unavailable.") {
				t.Fatalf("foreign %s returned HTTP %d instead of generic 404", item.operation, status)
			}
			for _, protected := range []string{id, baseline.Descriptor.Digest, receipt.ID, frozen.ID, "ISOLATED-RUN", "ISOLATED-RESULT"} {
				if strings.Contains(foreign, protected) {
					t.Fatalf("foreign %s disclosed a protected record key or content", item.operation)
				}
			}
			recordThresholdHTTPInstance(t, item.id, map[string]any{"requiredOperation": item.operation}, "No foreign data or mutation is exposed")
		})
	}
}

func TestFollowupCandidateFormRejectsAnUnknownManifestField(t *testing.T) {
	app, visit, id := startTestBrowser(t)
	client, csrf := loginSession(t, visit, "reviewer")
	_, _, approved, err := app.Candidate("reviewer", "pilot", id)
	if err != nil {
		t.Fatal(err)
	}
	path := visit + "/p/pilot/candidates"
	values := url.Values{
		"_csrf": {csrf}, "release": {"R2"}, "version": {"2"}, "baseline": {approved.Descriptor.Digest},
		"build": {strings.Repeat("b", 64)}, "environment": {"E1"},
		"prompt": {"not_applicable"}, "model": {"not_applicable"}, "knowledge": {"not_applicable"},
	}
	values.Set("unexpected_manifest_field", "fixture-value")
	if _, status := post(t, client, path, values); status != http.StatusUnprocessableEntity {
		t.Fatalf("candidate creation with an unknown manifest field returned HTTP %d", status)
	}
	all, err := app.Candidates("reviewer", "pilot")
	if err != nil || len(all) != 1 || all[0].ID != id {
		t.Fatalf("unknown field rejection did not preserve the candidate catalog: %v", err)
	}
	values.Del("unexpected_manifest_field")
	if _, status := post(t, client, path, values); status != http.StatusSeeOther {
		t.Fatalf("the same valid known fields were not accepted: HTTP %d", status)
	}
	all, err = app.Candidates("reviewer", "pilot")
	if err != nil || len(all) != 2 {
		t.Fatalf("valid candidate control was not stored: %v", err)
	}
	recordThresholdHTTPInstance(t, "AC-003-04.P002", map[string]any{"defect": "unknown field"}, "Rejected")
}
