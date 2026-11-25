package browser_test

import (
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"release-evidence-board/internal/board"
)

func TestBrowserForksApprovesAndDownloadsExactDraftRevision(t *testing.T) {
	app, visit, candidate := startTestBrowser(t)
	client, csrf := loginSession(t, visit, "reviewer")
	_, _, approved, err := app.Candidate("reviewer", "pilot", candidate)
	if err != nil {
		t.Fatal(err)
	}
	base := visit + "/p/pilot"
	for _, fixture := range []struct {
		name   string
		values url.Values
		status int
	}{
		{"unknown fields", url.Values{"unexpected": {"value"}}, 422},
		{"malformed new baseline", url.Values{"baseline_id": {"/invalid/"}}, 422},
		{"missing baseline source", url.Values{"baseline": {strings.Repeat("0", 64)}}, 404},
		{"missing draft source", url.Values{"draft": {"D-999"}}, 404},
		{"ambiguous derivation", url.Values{"baseline": {approved.Descriptor.Digest}, "draft": {approved.DraftID}}, 422},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			fixture.values.Set("_csrf", csrf)
			if _, status := post(t, client, base+"/drafts", fixture.values); status != fixture.status {
				t.Fatalf("draft creation returned HTTP %d, expected %d", status, fixture.status)
			}
		})
	}
	draftPath := submitRedirect(t, client, base+"/drafts", csrf, url.Values{"baseline": {approved.Descriptor.Digest}})
	draftID := strings.TrimPrefix(draftPath, "/p/pilot/drafts/")
	draft := loadBrowserDraft(t, app, draftID)
	if draft.Baseline.ID != "B1" || draft.Baseline.Revision != 2 || draft.ParentBaseline != approved.Descriptor.Digest {
		t.Fatal("derived draft did not retain approved baseline lineage and advance its revision")
	}
	for _, copy := range []struct {
		id       string
		name     string
		revision int
	}{
		{"", "B1", 3},
		{"IndependentCopy", "IndependentCopy", 1},
	} {
		path := submitRedirect(t, client, base+"/drafts", csrf, url.Values{"draft": {draftID}, "baseline_id": {copy.id}})
		fork := loadBrowserDraft(t, app, strings.TrimPrefix(path, "/p/pilot/drafts/"))
		if fork.ParentDraft != draftID || fork.Baseline.ID != copy.name || fork.Baseline.Revision != copy.revision {
			t.Fatalf("incorrect copy lineage for %q", copy.name)
		}
	}
	for _, route := range []string{"/drafts/D-999", "/baselines/absent", "/candidates/new?predecessor=C-absent"} {
		if _, status := response(t, client.Get, base+route); status != http.StatusNotFound {
			t.Fatalf("unavailable authoring record returned %d at %s", status, route)
		}
	}
	for _, values := range []url.Values{{"revision": {"invalid"}}, {"revision": {"1"}, "unknown": {"value"}}} {
		values.Set("_csrf", csrf)
		if _, status := post(t, client, visit+draftPath+"/approve", values); status != 422 {
			t.Fatalf("approval accepted malformed fields with status %d", status)
		}
	}
	if path := submitRedirect(t, client, visit+draftPath+"/approve", csrf, url.Values{"revision": {"1"}}); path != draftPath {
		t.Fatalf("approval led to a different draft %s", path)
	}
	frozen := loadBrowserDraft(t, app, draftID)
	artifact, err := app.Baseline("reviewer", "pilot", frozen.ApprovedDigest)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor, status := response(t, client.Get, base+"/baselines/"+artifact.Descriptor.Digest); status != http.StatusOK || descriptor != artifact.Descriptor.Text {
		t.Fatal("descriptor download differed from the exact approved canonical bytes")
	}
	if body, status := response(t, client.Get, visit+draftPath); status != http.StatusOK || !strings.Contains(body, artifact.Descriptor.Digest) || strings.Contains(body, "Approve saved draft") {
		t.Fatal("approved draft did not visibly lock its revision")
	}
	frozenFields := draftForm(csrf, frozen)
	if _, status := post(t, client, visit+draftPath, frozenFields); status != http.StatusConflict {
		t.Fatalf("approved draft remained editable with status %d", status)
	}
}

func TestBrowserDraftEditsEnforceRevisionValidateFieldsAndPreserveOrder(t *testing.T) {
	app, visit, candidate := startTestBrowser(t)
	client, csrf := loginSession(t, visit, "reviewer")
	_, _, approved, err := app.Candidate("reviewer", "pilot", candidate)
	if err != nil {
		t.Fatal(err)
	}
	base := visit + "/p/pilot"
	path := submitRedirect(t, client, base+"/drafts", csrf, url.Values{"baseline": {approved.Descriptor.Digest}})
	id := strings.TrimPrefix(path, "/p/pilot/drafts/")
	draft := loadBrowserDraft(t, app, id)
	for _, fixture := range []struct {
		name   string
		fields url.Values
		omit   string
		status int
	}{
		{"malformed revision", url.Values{"revision": {"invalid"}}, "", 409},
		{"stale revision", url.Values{"revision": {"0"}}, "", 409},
		{"unexpected fields for add", url.Values{"intent": {"add_behavior"}}, "", 422},
		{"malformed index", url.Values{"behavior_index": {"invalid"}}, "", 422},
		{"negative index", url.Values{"behavior_index": {"-1"}}, "", 422},
		{"missing index position", url.Values{"behavior_index": {"1"}}, "", 422},
		{"remove with invalid checkbox", url.Values{"intent": {"remove_behavior"}, "must_pass_0": {"false"}}, "", 422},
		{"missing behavior text", nil, "text", 422},
		{"malformed pass mark", url.Values{"minimum": {"decimal"}}, "", 422},
		{"malformed maximum age", url.Values{"max_age": {"decimal"}}, "", 422},
		{"missing test ID", nil, "test_0", 422},
		{"missing test expected outcome", nil, "expected_0", 422},
		{"malformed test planned attempts", url.Values{"attempts_0": {"decimal"}}, "", 422},
		{"malformed removal index", url.Values{"intent": {"remove_test"}, "test_remove": {"invalid"}}, "", 422},
		{"negative removal index", url.Values{"intent": {"remove_test"}, "test_remove": {"-1"}}, "", 422},
		{"out of range removal index", url.Values{"intent": {"remove_test"}, "test_remove": {"1"}}, "", 422},
		{"unrecognized intent", url.Values{"intent": {"invalid"}}, "", 422},
		{"inherited behavior rekey", url.Values{"behavior_id": {"renamed"}}, "", 422},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			values := draftForm(csrf, draft)
			for field, value := range fixture.fields {
				values[field] = value
			}
			if fixture.omit != "" {
				values.Del(fixture.omit)
			}
			if _, status := post(t, client, visit+path, values); status != fixture.status {
				t.Fatalf("draft edit returned HTTP %d, expected %d", status, fixture.status)
			}
			after := loadBrowserDraft(t, app, id)
			if after.Revision != 1 || len(after.Baseline.Behaviors) != 1 || after.Baseline.Behaviors[0].ID != "BEH1" || len(after.Baseline.Behaviors[0].Tests) != 1 {
				t.Fatal("rejected form changed the saved roster")
			}
		})
	}
	if _, status := post(t, client, base+"/drafts/D-999", draftForm(csrf, draft)); status != http.StatusNotFound {
		t.Fatalf("editing unavailable draft returned HTTP %d", status)
	}
	addTest := draftForm(csrf, draft)
	addTest.Set("intent", "add_test")
	submitRedirect(t, client, visit+path, csrf, addTest)
	draft = loadBrowserDraft(t, app, id)
	tests := draft.Baseline.Behaviors[0].Tests
	if len(tests) != 2 || tests[0].ID != "T1" || tests[1].ID != "BEH1-T2" || tests[1].PlannedAttempts != 1 {
		t.Fatal("adding a test changed existing slot ordering or default attempts")
	}
	removeTest := draftForm(csrf, draft)
	removeTest.Set("intent", "remove_test")
	removeTest.Set("test_remove", "1")
	submitRedirect(t, client, visit+path, csrf, removeTest)
	draft = loadBrowserDraft(t, app, id)
	submitRedirect(t, client, visit+path, csrf, url.Values{"revision": {strconv.FormatUint(draft.Revision, 10)}, "intent": {"add_behavior"}})
	draft = loadBrowserDraft(t, app, id)
	if draft.Revision != 4 || len(draft.Baseline.Behaviors) != 2 || draft.Baseline.Behaviors[1].ID != "B-2" {
		t.Fatal("adding a behavior did not retain the original roster and advance the draft")
	}
	removeBehavior := draftForm(csrf, draft)
	removeBehavior.Set("behavior_index", "1")
	removeBehavior.Set("intent", "remove_behavior")
	submitRedirect(t, client, visit+path, csrf, removeBehavior)
	draft = loadBrowserDraft(t, app, id)
	if draft.Revision != 5 || len(draft.Baseline.Behaviors) != 1 || draft.Baseline.Behaviors[0].ID != "BEH1" || len(draft.Baseline.Behaviors[0].Tests) != 1 {
		t.Fatal("removing a behavior altered the retained roster")
	}
}

func TestBrowserCandidateFormSurvivesValidationAndReusesExactMaterial(t *testing.T) {
	app, visit, id := startTestBrowser(t)
	client, csrf := loginSession(t, visit, "reviewer")
	base := visit + "/p/pilot"
	candidate, _, approved, err := app.Candidate("reviewer", "pilot", id)
	if err != nil {
		t.Fatal(err)
	}
	get := base + "/candidates/new?predecessor=" + id + "&baseline=" + approved.Descriptor.Digest
	if form, status := response(t, client.Get, get); status != http.StatusOK || !strings.Contains(form, `value="1.0"`) || !strings.Contains(form, candidate.DisplayVersion) {
		t.Fatal("successor form did not prefill fields from its exact predecessor")
	}
	viewer, _ := loginSession(t, visit, "viewer")
	if _, status := response(t, viewer.Get, base+"/candidates/new"); status != http.StatusForbidden {
		t.Fatalf("viewer reached candidate authoring with HTTP %d", status)
	}
	input := url.Values{"_csrf": {csrf}, "release": {"R1"}, "version": {"1.0"}, "display": {"Review this candidate"}, "baseline": {approved.Descriptor.Digest},
		"build": {"invalid"}, "prompt": {"not_applicable"}, "model": {"not_applicable"}, "knowledge": {"not_applicable"}, "environment": {"E1"}}
	input.Set("unexpected", "ignored")
	if _, status := post(t, client, base+"/candidates", input); status != 422 {
		t.Fatalf("malformed submission accepted with HTTP %d", status)
	}
	input.Del("unexpected")
	if form, status := post(t, client, base+"/candidates", input); status != 422 || !strings.Contains(form, "64 lowercase hexadecimal") || !strings.Contains(form, "Review this candidate") {
		t.Fatal("invalid manifest did not preserve the attempted form and validation error")
	}
	input.Set("build", strings.Repeat("a", 64))
	path := submitRedirect(t, client, base+"/candidates", csrf, input)
	if path != "/p/pilot/candidates/"+id+"?reused=1" {
		t.Fatalf("same material was assigned a new board %q", path)
	}
	if candidates, err := app.Candidates("reviewer", "pilot"); err != nil || len(candidates) != 1 {
		t.Fatal("invalid or identical submissions created additional candidate records")
	}
}

func submitRedirect(t *testing.T, client *http.Client, endpoint, csrf string, fields url.Values) string {
	t.Helper()
	fields.Set("_csrf", csrf)
	res, err := client.PostForm(endpoint, fields)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	location := res.Header.Get("Location")
	if res.StatusCode != http.StatusSeeOther || location == "" {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("expected redirect for %s, got HTTP %d %s", endpoint, res.StatusCode, body)
	}
	return location
}

func loadBrowserDraft(t *testing.T, app *board.Board, id string) board.Draft {
	t.Helper()
	draft, err := app.Draft("reviewer", "pilot", id)
	if err != nil {
		t.Fatal(err)
	}
	return draft
}
