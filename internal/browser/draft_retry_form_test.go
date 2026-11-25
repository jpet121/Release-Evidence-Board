package browser_test

import (
	"html"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"release-evidence-board/internal/board"
)

func TestBrowserDraftParseErrorRetainsAndSafelyRetriesUserWork(t *testing.T) {
	app, visit, id := startTestBrowser(t)
	client, csrf := loginSession(t, visit, "reviewer")
	_, _, approved, err := app.Candidate("reviewer", "pilot", id)
	if err != nil {
		t.Fatal(err)
	}
	draftPath := submitRedirect(t, client, visit+"/p/pilot/drafts", csrf, url.Values{"baseline": {approved.Descriptor.Digest}})
	draftID := strings.TrimPrefix(draftPath, "/p/pilot/drafts/")
	before := loadBrowserDraft(t, app, draftID)
	submitRedirect(t, client, visit+draftPath, csrf, url.Values{
		"intent": {"add_behavior"}, "revision": {strconv.FormatUint(before.Revision, 10)},
	})
	before = loadBrowserDraft(t, app, draftID)
	fields := draftForm(csrf, before)
	text := "Keep this draft </textarea><script>alert(71)</script> text"
	const expected = "Use the changed expected outcome"
	fields.Set("text", text)
	fields.Set("expected_0", expected)
	fields.Set("intent", "add_test")
	fields.Set("attempts_0", "1.5")
	fields.Set("test_remove", "0")
	fields.Del("required")
	body, status := post(t, client, visit+draftPath, fields)
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, "enter an integer planned-attempt count") {
		t.Fatalf("invalid integer did not bring back the explicit edit error: HTTP %d", status)
	}
	after := loadBrowserDraft(t, app, draftID)
	if after.Revision != before.Revision || after.Baseline.Behaviors[0].Text != before.Baseline.Behaviors[0].Text {
		t.Fatal("rejected form changed the saved draft")
	}
	for _, want := range []string{html.EscapeString(text), expected, `name="attempts_0" value="1.5"`, "Save behavior"} {
		if !strings.Contains(body, want) {
			t.Errorf("draft validation response lost submitted content or retry control %q", want)
		}
	}
	if strings.Contains(body, "<script>alert(71)</script>") ||
		!strings.Contains(body, `name="required" value="1" > Required behavior`) ||
		!strings.Contains(body, `<option value="0" selected>T1</option>`) ||
		strings.Count(body, html.EscapeString(text)) != 1 ||
		!strings.Contains(body, `name="behavior_id" value="B-2"`) {
		t.Fatal("retry form executed markup or silently re-selected the cleared checkbox")
	}
	fields.Set("intent", "save_behavior")
	fields.Set("attempts_0", "1")
	submitRedirect(t, client, visit+draftPath, csrf, fields)
	saved := loadBrowserDraft(t, app, draftID)
	behavior := saved.Baseline.Behaviors[0]
	if behavior.Text != text || behavior.Required || behavior.Tests[0].ExpectedOutcome != expected {
		t.Fatalf("corrected retry did not save the edited user work: %+v", behavior)
	}
}

func TestBrowserStaleDraftKeepsSubmittedWorkAndRequiresReviewOfLatestRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "draft-conflict.db")
	settings := board.Settings{ProjectID: "pilot", ProductID: "DEMO"}
	app, err := board.Open(path, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	if err := app.Bootstrap("reviewer", browserPassword); err != nil {
		t.Fatal(err)
	}
	if err := app.SetAccount("reviewer", "earlier-editor", browserPassword, []board.Role{board.Contributor}); err != nil {
		t.Fatal(err)
	}
	draft, err := app.CreateDraft("reviewer", "pilot", board.Baseline{
		ID: "CONFLICT", Revision: 1, ProductID: "DEMO",
		Behaviors: []board.Behavior{
			{ID: "FIRST", Revision: 1, Text: "Behavior before the edited row",
				Policy: board.Policy{ID: "FIRST-POLICY", Revision: 1, MaxAgeHours: 24}},
			{ID: "BEH1", Revision: 1, Required: true, Text: "Original shared behavior",
				Policy: board.Policy{ID: "P1", Revision: 1, MaxAgeHours: 24, MinPassBasisPoints: 10000},
				Tests: []board.TestPlan{
					{ID: "T1", ExpectedOutcome: "Original first test", PlannedAttempts: 1, MustPass: true},
					{ID: "T2", ExpectedOutcome: "Original second test", PlannedAttempts: 1, MustPass: true},
				}},
		},
	}, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	visit := serveBrowserTestBoard(t, app)
	draftPath := "/p/pilot/drafts/" + draft.ID
	earlier, earlierCSRF := loginSession(t, visit, "earlier-editor")
	later, laterCSRF := loginSession(t, visit, "reviewer")
	oldRevision := strconv.FormatUint(draft.Revision, 10)
	page, status := response(t, earlier.Get, visit+draftPath)
	if status != http.StatusOK || !strings.Contains(page, `name="revision" value="`+oldRevision+`"`) ||
		!strings.Contains(page, `name="behavior_index" value="1"`) || !strings.Contains(page, `name="test_1" value="T2"`) {
		t.Fatalf("the earlier editor did not load the actual pre-conflict form: HTTP %d", status)
	}
	earlierDraft := draft
	earlierDraft.Baseline.Behaviors = draft.Baseline.Behaviors[1:]
	stale := draftForm(earlierCSRF, earlierDraft)
	const text = "My edits </textarea><script>draftConflict()</script> stay available"
	const expectFirst = "My first expected outcome"
	const expectSecond = "My second outcome <img src=x onerror=draftConflict()>"
	stale.Set("behavior_index", "1")
	stale.Set("text", text)
	stale.Set("policy_id", "MY-POLICY")
	stale.Set("minimum", "8750")
	stale.Set("max_age", "36")
	stale.Set("attempts_0", "3")
	stale.Set("attempts_1", "2")
	stale.Set("expected_0", expectFirst)
	stale.Set("expected_1", expectSecond)
	stale.Del("required")
	stale.Del("must_pass_1")

	removeBehavior := draftForm(laterCSRF, draft)
	removeBehavior.Set("intent", "remove_behavior")
	submitRedirect(t, later, visit+draftPath, laterCSRF, removeBehavior)
	removeTest := draftForm(laterCSRF, loadBrowserDraft(t, app, draft.ID))
	removeTest.Set("intent", "remove_test")
	removeTest.Set("test_remove", "1")
	removeTest.Set("text", "The later editor's saved behavior")
	submitRedirect(t, later, visit+draftPath, laterCSRF, removeTest)
	latest := loadBrowserDraft(t, app, draft.ID)
	if latest.Revision != draft.Revision+2 || len(latest.Baseline.Behaviors) != 1 ||
		latest.Baseline.Behaviors[0].ID != "BEH1" || len(latest.Baseline.Behaviors[0].Tests) != 1 {
		t.Fatal("the later editor did not actually remove and reindex the saved roster")
	}

	page, status = post(t, earlier, visit+draftPath, stale)
	if status != http.StatusConflict {
		t.Fatalf("the stale POST returned HTTP %d, want 409", status)
	}
	for _, want := range []string{
		"Submitted draft revision " + oldRevision, "Copy your edits", "Review latest draft",
		`href="` + draftPath + `" target="_blank" rel="noopener"`,
		html.EscapeString(text), "MY-POLICY", "8750", "36",
		"<dt>Required behavior</dt><dd>No</dd>",
		"<dt>Planned attempts</dt><dd>3</dd>", "<dt>Planned attempts</dt><dd>2</dd>",
		"<dt>Must pass</dt><dd>Yes</dd>", "<dt>Must pass</dt><dd>No</dd>",
		expectFirst, html.EscapeString(expectSecond), "T2",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("stale draft conflict lost submitted work or review action %q", want)
		}
	}
	if strings.Contains(page, "<script>draftConflict()") || strings.Contains(page, "<img src=x") ||
		strings.Contains(page, `action="`+draftPath+`"`) || strings.Contains(page, "name=\"revision\"") {
		t.Error("stale conflict response interpreted submitted text or offered a draft mutation")
	}
	page, status = response(t, earlier.Get, visit+draftPath)
	if status != http.StatusOK || !strings.Contains(page, "The later editor&#39;s saved behavior") ||
		!strings.Contains(page, `name="revision" value="`+strconv.FormatUint(latest.Revision, 10)+`"`) ||
		strings.Contains(page, expectFirst) {
		t.Errorf("explicit review did not open the actual latest revision: HTTP %d", status)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := board.Open(path, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got := loadBrowserDraft(t, reopened, draft.ID)
	if !reflect.DeepEqual(got, latest) {
		t.Fatal("the stale submission changed the later editor's saved draft after reopen")
	}
}
