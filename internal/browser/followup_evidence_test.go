package browser_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

const followupHeader = "schema_version,record_type,result_id,product_id,release_id,manifest_digest,baseline_digest,behavior_id,behavior_revision,policy_id,policy_revision,test_id,run_id,attempt_index,finished_at,run_finished_at,outcome,source_uri,failure_excerpt\n"

// AC-009-01/03. Required counts describe the entire approved roster regardless
// of the current visual filter. The optional failure remains inspectable.
func TestFollowupBrowserStatusFiltersKeepRequiredCountsAndShowFailureHistory(t *testing.T) {
	app, baseURL, _ := startTestBrowser(t)
	viewer, _ := loginSession(t, baseURL, "viewer")
	contents := board.Baseline{ProductID: "DEMO", ID: "mixed", Revision: 1}
	for _, id := range []string{"PASS", "FAIL", "STALE", "UNKNOWN", "OPTIONAL"} {
		contents.Behaviors = append(contents.Behaviors, board.Behavior{
			ID: "B-" + id, Required: id != "OPTIONAL", Revision: 1, Text: "Evidence for " + id,
			Policy: board.Policy{ID: "P-" + id, Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 1},
			Tests:  []board.TestPlan{{ID: "T-" + id, ExpectedOutcome: "Check recorded output", MustPass: id != "OPTIONAL", PlannedAttempts: 1}},
		})
	}
	draft, err := app.CreateDraft("reviewer", "pilot", contents, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := app.Approve("reviewer", "pilot", draft.ID, draft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	manifest := board.Manifest{ProductID: "DEMO", ReleaseID: "R2", ApplicationVersion: "2.0", BuildDigest: strings.Repeat("b", 64),
		PromptDigest: "not_applicable", ModelConfigDigest: "not_applicable", KnowledgeDigest: "not_applicable", EnvironmentID: "E1"}
	candidate, reused, err := app.CreateCandidate("reviewer", "pilot", board.CandidateInput{Manifest: manifest, BaselineDigest: approved.Descriptor.Digest})
	if err != nil || reused {
		t.Fatalf("mixed-roster candidate fixture failed: %v", err)
	}
	now := time.Now().UTC()
	fresh := board.FormatUTC(now.Add(-5 * time.Minute))
	old := board.FormatUTC(now.Add(-2 * time.Hour))
	prior := board.FormatUTC(now.Add(-6 * time.Minute))
	missing := []string{"reb-results/1.3", "RUN_ONLY", "", "DEMO", "R2", candidate.ManifestDigest, candidate.BaselineDigest,
		"B-UNKNOWN", "1", "P-UNKNOWN", "1", "", "MISSING-LATE", "", "", fresh, "", "", ""}
	raw := followupBrowserCSV(t,
		followupBrowserResult(candidate, "R2", "B-PASS", "P-PASS", "T-PASS", "RUN-PASS", "PASS1", fresh, "PASS"),
		followupBrowserResult(candidate, "R2", "B-FAIL", "P-FAIL", "T-FAIL", "FAIL-OLD", "FAIL1", prior, "FAIL"),
		followupBrowserResult(candidate, "R2", "B-FAIL", "P-FAIL", "T-FAIL", "FAIL-RERUN", "FAIL2", fresh, "PASS"),
		followupBrowserResult(candidate, "R2", "B-STALE", "P-STALE", "T-STALE", "RUN-STALE", "STALE1", old, "PASS"),
		followupBrowserResult(candidate, "R2", "B-UNKNOWN", "P-UNKNOWN", "T-UNKNOWN", "COMPLETE-TIE", "UNKNOWN1", fresh, "PASS"),
		followupBrowserResult(candidate, "R2", "B-OPTIONAL", "P-OPTIONAL", "T-OPTIONAL", "RUN-OPTIONAL", "OPTIONAL1", fresh, "FAIL"), missing)
	commitFollowupBrowserCSV(t, app, candidate.ID, raw)
	path := baseURL + "/p/pilot/candidates/" + candidate.ID
	for _, check := range []struct {
		name, query string
		visible     []string
	}{
		{"pass", "status=PASS", []string{"PASS"}},
		{"fail", "status=FAIL", []string{"FAIL", "OPTIONAL"}},
		{"stale", "status=STALE", []string{"STALE"}},
		{"unknown", "status=UNKNOWN", []string{"UNKNOWN"}},
		{"historical must-pass failure", "status=FAIL&reason=MUST_PASS_FAILED", []string{"FAIL"}},
		{"reason and status disagree", "status=PASS&reason=MUST_PASS_FAILED", nil},
	} {
		t.Run(check.name, func(t *testing.T) {
			body, status := response(t, viewer.Get, path+"?"+check.query)
			if status != http.StatusOK {
				t.Fatalf("filter returned HTTP %d", status)
			}
			if !strings.Contains(body, `class="status BLOCKED"`) || strings.Contains(body, `class="status SUFFICIENT"`) {
				t.Fatal("filter changed the aggregate decision")
			}
			for _, count := range []string{"✓ PASS 1", "× FAIL 1", "◷ STALE 1", "? UNKNOWN 1"} {
				if !strings.Contains(body, count) {
					t.Errorf("required roster count disappeared: %s", count)
				}
			}
			for _, id := range []string{"PASS", "FAIL", "STALE", "UNKNOWN", "OPTIONAL"} {
				wanted := false
				for _, selected := range check.visible {
					wanted = wanted || selected == id
				}
				if strings.Contains(body, `id="behavior-B-`+id+`"`) != wanted {
					t.Errorf("behavior %s visible = %t expected", id, wanted)
				}
			}
			if (len(check.visible) == 0) != strings.Contains(body, "No behaviors match these filters.") {
				t.Error("empty filter did not explain that no behaviors matched")
			}
			if t.Failed() {
				return
			}
			filterCase := map[string]string{"fail": ".P001", "stale": ".P002", "unknown": ".P003", "pass": ".P004"}
			if suffix, ok := filterCase[check.name]; ok {
				recordThresholdHTTPInstance(t, "AC-009-03"+suffix, map[string]any{"statusFilter": strings.ToUpper(check.name)}, "Counts reconcile to required roster")
			}
		})
	}
	failed, status := response(t, viewer.Get, path+"/behaviors?id=B-FAIL&status=FAIL&reason=MUST_PASS_FAILED")
	if status != http.StatusOK || !strings.Contains(failed, "Current run <strong>FAIL-RERUN</strong>") ||
		!strings.Contains(failed, `href="#run-FAIL-OLD"`) || !strings.Contains(failed, "MUST_PASS_FAILED") ||
		!strings.Contains(failed, candidate.ManifestDigest) || !strings.Contains(failed, candidate.BaselineDigest) || !strings.Contains(failed, "Policy P-FAIL revision 1") {
		t.Fatal("behavior detail failed to distinguish the current rerun and retained blocker")
	}
	tied, status := response(t, viewer.Get, path+"/behaviors?id=B-UNKNOWN")
	if status != http.StatusOK || !strings.Contains(tied, "Current run <strong>MISSING-LATE</strong>") ||
		!strings.Contains(tied, "incomplete run was selected at a tied finish time") || !strings.Contains(tied, "COMPLETE-TIE") {
		t.Fatal("behavior detail omitted the equal-finish incomplete selection or alternate run")
	}
	t.Log("AC-009-03 filters=PASS,FAIL,STALE,UNKNOWN required_counts=1,1,1,1; AC-009-01 current=FAIL-RERUN blocker=FAIL-OLD reason=MUST_PASS_FAILED")
}

// AC-009-02/06 and AC-010-01/02. User-facing artifact links preserve original
// bytes under the assigned role, and direct foreign-project paths disclose none.
func TestFollowupBrowserExactEvidenceDownloadsAndForeignObjectDenial(t *testing.T) {
	app, baseURL, id := startTestBrowser(t)
	viewer, _ := loginSession(t, baseURL, "viewer")
	reviewer, csrf := loginSession(t, baseURL, "reviewer")
	candidate, _, _, err := app.Candidate("reviewer", "pilot", id)
	if err != nil {
		t.Fatal(err)
	}
	stamp := board.FormatUTC(time.Now().UTC().Add(-3 * time.Minute))
	row := followupBrowserResult(candidate, "R1", "BEH1", "P1", "T1", "LOCAL-RUN", "LOCAL-RESULT", stamp, "PASS")
	row[17], row[18] = "https://example.invalid/vendor-source", "<strong>Literal observation & trace</strong>"
	raw := followupBrowserCSV(t, row)
	first := commitFollowupBrowserCSV(t, app, id, raw)
	quoted := bytes.Replace(raw, []byte(",PASS,"), []byte(`,"PASS",`), 1)
	if bytes.Equal(raw, quoted) {
		t.Fatal("test fixture did not change the CSV quoting")
	}
	noop := commitFollowupBrowserCSV(t, app, id, quoted)
	if noop.LedgerHighWater != 1 || noop.ID == first.ID || len(noop.AddedRuns) != 0 || noop.Hash != fixtureHash(quoted) {
		t.Fatal("equivalent quoting did not produce a distinct no-op receipt")
	}
	assessed, err := app.Assess("reviewer", "pilot", id)
	if err != nil || len(assessed.Behaviors) != 1 || len(assessed.Behaviors[0].Runs) != 1 || len(assessed.Behaviors[0].Runs[0].Rows) != 1 || assessed.Behaviors[0].Runs[0].Rows[0].ResultID() != "LOCAL-RESULT" {
		t.Fatalf("the no-op receipt changed the single original run or result: %v", err)
	}
	path := "/p/pilot/candidates/" + id
	view, status := response(t, viewer.Get, baseURL+path+"/behaviors?id=BEH1")
	if status != http.StatusOK || !strings.Contains(view, "Unarchived source link") ||
		!strings.Contains(view, "https://example.invalid/vendor-source") || !strings.Contains(view, "LOCAL-RESULT") ||
		!strings.Contains(view, "/p/pilot/receipts/"+first.ID) ||
		!strings.Contains(view, `&lt;strong&gt;Literal observation &amp; trace&lt;/strong&gt;`) ||
		strings.Contains(view, row[18]) {
		t.Fatal("retained row did not expose escaped local evidence and its unarchived reference")
	}
	receiptPath := "/p/pilot/receipts/" + noop.ID
	view, status = response(t, viewer.Get, baseURL+receiptPath)
	if status != http.StatusOK || !strings.Contains(view, "No new runs were added.") ||
		!strings.Contains(view, "Identical retained runs LOCAL-RUN") || !strings.Contains(view, "Prior receipt "+first.ID) {
		t.Fatal("no-op receipt lost the previous exact import link")
	}
	const canonicalPrefix = `["reb-manifest/1","DEMO","R1","1.0","`
	wantManifest := canonicalPrefix + strings.Repeat("a", 64) + `","not_applicable","not_applicable","not_applicable","E1"]`
	manifest, headers, status := readFollowupArtifact(t, viewer, baseURL+path+"/manifest")
	if status != http.StatusOK || string(manifest) != wantManifest || headers.Get("Content-Type") != "application/json" {
		t.Fatal("manifest download differs from the canonical C01 bytes")
	}
	source, headers, status := readFollowupArtifact(t, viewer, baseURL+receiptPath+"/source")
	if status != http.StatusOK || !bytes.Equal(source, quoted) || headers.Get("Content-Type") != "text/csv; charset=utf-8" {
		t.Fatal("receipt source download changed original CSV quoting")
	}
	assertFollowupFilename(t, headers, "original-"+noop.ID+".csv")
	recordThresholdHTTPInstance(t, "AC-005-04.P002", map[string]any{"newFileTransform": "change CSV quoting"}, "No-op receipt; no new run/result")
	frozenResponse, err := reviewer.PostForm(baseURL+path+"/reports", url.Values{"_csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	reportPath := frozenResponse.Header.Get("Location")
	if err := frozenResponse.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if frozenResponse.StatusCode != http.StatusSeeOther || !strings.HasPrefix(reportPath, "/p/pilot/reports/RPT-") {
		t.Fatalf("frozen assessment created wrong HTTP result %d", frozenResponse.StatusCode)
	}
	frozen, headers, status := readFollowupArtifact(t, viewer, baseURL+reportPath+"?download=1")
	if status != http.StatusOK || !bytes.Contains(frozen, []byte("Historical assessment · SUFFICIENT")) ||
		!bytes.Contains(frozen, []byte(`&lt;strong&gt;Literal observation &amp; trace&lt;/strong&gt;`)) || bytes.Contains(frozen, []byte(row[18])) {
		t.Fatal("HTML download lost historical status or escaped evidence")
	}
	assertFollowupFilename(t, headers, strings.TrimPrefix(reportPath, "/p/pilot/reports/")+".html")
	wantETag := fmt.Sprintf(`"%x"`, sha256.Sum256(frozen))
	if headers.Get("ETag") != wantETag {
		t.Fatal("HTTP report hash differs from the frozen attachment bytes")
	}
	changed := followupBrowserResult(candidate, "R1", "BEH1", "P1", "T1", "NEW-FAIL", "FAIL-RESULT", stamp, "FAIL")
	commitFollowupBrowserCSV(t, app, id, followupBrowserCSV(t, changed))
	assessed, err = app.Assess("reviewer", "pilot", id)
	if err != nil || assessed.Status != "BLOCKED" || assessed.LedgerHighWater != 2 {
		t.Fatalf("later failure did not change the live assessment: %v", err)
	}
	again, headers, status := readFollowupArtifact(t, viewer, baseURL+reportPath+"?download=1")
	if status != http.StatusOK || !bytes.Equal(again, frozen) || headers.Get("ETag") != wantETag {
		t.Fatal("later import changed an already frozen report")
	}
	recordThresholdHTTPInstance(t, "AC-010-02.P001", map[string]any{}, "Report bytes and hash unchanged")
	for _, access := range []struct{ child, id, operation, endpoint string }{
		{"candidate_detail", "AC-001-03.P002", "candidate detail", path},
		{"manifest_descriptor_download", "AC-001-03.P004", "manifest descriptor download", path + "/manifest"},
		{"import_receipt", "AC-001-03.P006", "import receipt", receiptPath},
		{"retained_original_upload", "AC-001-03.P011", "retained original upload", receiptPath + "/source"},
		{"frozen_report_download", "AC-001-03.P013", "frozen report download", reportPath + "?download=1"},
	} {
		t.Run(access.child, func(t *testing.T) {
			_, _, localStatus := readFollowupArtifact(t, viewer, baseURL+access.endpoint)
			if localStatus != http.StatusOK {
				t.Fatalf("assigned Viewer could not read the corresponding local object: HTTP %d", localStatus)
			}
			foreign := strings.Replace(access.endpoint, "/p/pilot/", "/p/foreign/", 1)
			body, headers, status := readFollowupArtifact(t, viewer, baseURL+foreign)
			if status != http.StatusNotFound || headers.Get("Content-Disposition") != "" || !bytes.Contains(body, []byte("The requested record is unavailable.")) {
				t.Fatal("foreign object response was not the generic unavailable page")
			}
			for _, secret := range []string{"LOCAL-RESULT", candidate.ID, candidate.ManifestDigest, noop.ID, strings.TrimPrefix(reportPath, "/p/pilot/reports/")} {
				if bytes.Contains(body, []byte(secret)) {
					t.Fatal("foreign workspace request exposed protected fixture content")
				}
			}
			recordThresholdHTTPInstance(t, access.id, map[string]any{"requiredOperation": access.operation}, "No foreign data or mutation is exposed")
		})
	}
	t.Log("C01 manifest=exact; original_source=exact; AC-010-02 html_unchanged=true etag_unchanged=true; AC-001-03 known_foreign_object_paths_denied=5")
}

func followupBrowserResult(candidate board.Candidate, release, behavior, policy, test, run, result, stamp, outcome string) []string {
	return []string{"reb-results/1.3", "RESULT", result, "DEMO", release, candidate.ManifestDigest, candidate.BaselineDigest,
		behavior, "1", policy, "1", test, run, "1", stamp, stamp, outcome, "", ""}
}

func followupBrowserCSV(t *testing.T, rows ...[]string) []byte {
	t.Helper()
	var data bytes.Buffer
	data.WriteString(followupHeader)
	if err := csv.NewWriter(&data).WriteAll(rows); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func commitFollowupBrowserCSV(t *testing.T, app *board.Board, candidateID string, raw []byte) board.Receipt {
	t.Helper()
	preview, err := app.PreviewImport("reviewer", "pilot", candidateID, raw)
	if err != nil || preview.Token == "" || len(preview.Issues) != 0 {
		t.Fatalf("expected valid fixture CSV: %v, diagnostics=%v", err, preview.Issues)
	}
	receipt, err := app.CommitImport("reviewer", "pilot", preview.Token, raw)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func readFollowupArtifact(t *testing.T, client *http.Client, address string) ([]byte, http.Header, int) {
	t.Helper()
	response, err := client.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	if closeError := response.Body.Close(); err != nil || closeError != nil {
		t.Fatalf("read artifact: %v, %v", err, closeError)
	}
	return data, response.Header, response.StatusCode
}

func assertFollowupFilename(t *testing.T, header http.Header, name string) {
	t.Helper()
	typeName, parameters, err := mime.ParseMediaType(header.Get("Content-Disposition"))
	if err != nil || typeName != "attachment" || parameters["filename"] != name {
		t.Fatalf("download filename %q was not offered: %v", name, err)
	}
}
