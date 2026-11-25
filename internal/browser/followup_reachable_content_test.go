package browser_test

import (
	"bytes"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestBrowserIndependentThresholdReviewRendersAffectedCandidateAndRoles(t *testing.T) {
	fx := newThresholdHTTPFixture(t)
	receipt := fx.commitFailure(t)
	draft := fx.pendingDraft(t, "ordinary derived draft", "same")
	path := strings.TrimSuffix(fx.endpoint(draft), "/approve")
	page, status := response(t, fx.reducer.client.Get, fx.visit+path)
	if status != http.StatusOK || !strings.Contains(page, "Independent reviewer required") ||
		!strings.Contains(page, "BEH1 · 10000 to 5000 basis points · reduced by reviewer") ||
		!strings.Contains(page, "/p/pilot/candidates/"+fx.old.ID+"\">Affected candidate</a>") {
		t.Fatalf("pending threshold review did not name its actor, marks and exact blocked candidate: HTTP %d", status)
	}
	if err := fx.app.SetAccount(fx.reducer.actor, "contributor", browserPassword, []board.Role{board.Contributor}); err != nil {
		t.Fatal(err)
	}
	contributor, _ := loginSession(t, fx.visit, "contributor")
	page, status = response(t, contributor.Get, fx.visit+path)
	if status != http.StatusOK || !strings.Contains(page, "Independent reviewer required") ||
		!strings.Contains(page, "Save behavior") || strings.Contains(page, "Approve saved draft") {
		t.Fatalf("contributor view lost the threshold disclosure or gained approval: HTTP %d", status)
	}
	if err := fx.app.SetAccount(fx.reducer.actor, "viewer", browserPassword, []board.Role{board.Viewer}); err != nil {
		t.Fatal(err)
	}
	viewer, _ := loginSession(t, fx.visit, "viewer")
	page, status = response(t, viewer.Get, fx.visit+path)
	if status != http.StatusOK || !strings.Contains(page, "Two independent release checks") ||
		!strings.Contains(page, "Second check passes · 1 planned</li>") || strings.Contains(page, "Save behavior") {
		t.Fatalf("Viewer did not receive the read-only valid test plan: HTTP %d", status)
	}
	page, status = response(t, viewer.Get, fx.visit+"/p/pilot/candidates/"+fx.old.ID+"/reports")
	if status != http.StatusOK || !strings.Contains(page, "No reports have been frozen.") || strings.Contains(page, "Freeze assessment") {
		t.Fatalf("Viewer empty report list offered a freeze: HTTP %d", status)
	}
	fx.requirePreservedFailure(t, receipt)
}

func TestBrowserEvidenceTieAndNormalizedReplayKeepBothExactReceipts(t *testing.T) {
	fx := newAuthorityFixture(t)
	candidate, _, _, err := fx.app.Candidate(fx.reviewer.actor, "pilot", fx.candidateID)
	if err != nil {
		t.Fatal(err)
	}
	path := "/p/pilot/candidates/" + fx.candidateID
	stamp := board.FormatUTC(time.Now().UTC().Add(-time.Minute))
	complete := followupBrowserResult(candidate, "R1", "BEH1", "P1", "T1", "COMPLETE-TIE", "TIE-PASS", stamp, "PASS")
	empty := []string{"reb-results/1.3", "RUN_ONLY", "", "DEMO", "R1", candidate.ManifestDigest, candidate.BaselineDigest,
		"BEH1", "1", "P1", "1", "", "INCOMPLETE-TIE", "", "", stamp, "", "", ""}
	raw := followupBrowserCSV(t, complete, empty)
	preview := fx.send(t, fx.reviewer, path+"/preview", nil, raw)
	token := authorityPreviewToken.FindStringSubmatch(preview.body)
	if preview.status != http.StatusOK || len(token) != 2 ||
		!strings.Contains(preview.body, "incomplete run wins a finish-time tie") ||
		!strings.Contains(preview.body, "INCOMPLETE-TIE · Projected current run") ||
		!strings.Contains(preview.body, "RUN_ONLY · CSV row 3 · zero results") {
		t.Fatalf("actual tie preview did not disclose the conservative decision: HTTP %d", preview.status)
	}
	first := fx.send(t, fx.reviewer, path+"/commit", url.Values{"preview": {token[1]}}, nil)
	if first.status != http.StatusSeeOther || !strings.HasPrefix(first.location, "/p/pilot/receipts/I-") {
		t.Fatalf("confirmed tie evidence did not commit: HTTP %d, %q", first.status, first.location)
	}
	page, status := response(t, fx.reviewer.client.Get, fx.visit+path+"/behaviors/BEH1")
	if status != http.StatusOK || !strings.Contains(page, "an incomplete run was selected at a tied finish time") ||
		!strings.Contains(page, "RUN_ONLY · No result rows") || !strings.Contains(page, "COMPLETE-TIE") {
		t.Fatalf("committed tie lost the complete run or explicit empty run: HTTP %d", status)
	}
	quoted := bytes.Replace(raw, []byte(",RUN_ONLY,"), []byte(",\"RUN_ONLY\","), 1)
	preview = fx.send(t, fx.reviewer, path+"/preview", nil, quoted)
	token = authorityPreviewToken.FindStringSubmatch(preview.body)
	if preview.status != http.StatusOK || len(token) != 2 ||
		!strings.Contains(preview.body, "Identical retained runs COMPLETE-TIE INCOMPLETE-TIE") {
		t.Fatalf("same runs in distinct CSV bytes did not disclose retained identity: HTTP %d", preview.status)
	}
	duplicate := fx.send(t, fx.reviewer, path+"/commit", url.Values{"preview": {token[1]}}, nil)
	if duplicate.status != http.StatusSeeOther || duplicate.location == first.location {
		t.Fatalf("distinct exact CSV custody was not retained: HTTP %d, %q", duplicate.status, duplicate.location)
	}
	page, status = response(t, fx.reviewer.client.Get, fx.visit+duplicate.location)
	if status != http.StatusOK || !strings.Contains(page, "No new runs were added.") ||
		!strings.Contains(page, "Identical retained runs COMPLETE-TIE INCOMPLETE-TIE") ||
		!strings.Contains(page, "href=\""+first.location+"\">Prior receipt") {
		t.Fatalf("overlap receipt lost its retained run source or history link: HTTP %d", status)
	}
	for _, source := range []struct {
		path string
		raw  []byte
	}{{first.location, raw}, {duplicate.location, quoted}} {
		body, status := response(t, fx.reviewer.client.Get, fx.visit+source.path+"/source")
		if status != http.StatusOK || !bytes.Equal([]byte(body), source.raw) {
			t.Fatalf("receipt %s did not preserve its exact original file: HTTP %d", source.path, status)
		}
	}
	fx.requireDecision(t, board.Blocked, 2)
}

func TestBrowserFinalTestRemovalKeepsEditableBehavior(t *testing.T) {
	fx := newAuthorityFixture(t)
	draft, err := fx.app.CreateDraft(fx.reviewer.actor, "pilot", board.Baseline{
		ProductID: "DEMO", ID: "EDITABLE", Revision: 1,
		Behaviors: []board.Behavior{{
			ID: "OPTIONAL", Revision: 1, Text: "An optional test can be drafted later",
			Policy: board.Policy{ID: "OPTIONAL-POLICY", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
			Tests:  []board.TestPlan{{ID: "T-OPTIONAL", ExpectedOutcome: "Optional test", PlannedAttempts: 1}},
		}},
	}, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	path := "/p/pilot/drafts/" + draft.ID
	catalog, status := response(t, fx.reviewer.client.Get, fx.visit+"/p/pilot/baselines")
	if status != http.StatusOK || !strings.Contains(catalog, path+"\">EDITABLE · revision 1</a>") ||
		strings.Contains(catalog, "EDITABLE · revision 1</a> · reviewer · Approved") {
		t.Fatalf("real pending draft was lost or misrepresented as approved: HTTP %d", status)
	}
	fields := draftForm(fx.reviewer.csrf, draft)
	fields.Set("intent", "remove_test")
	fields.Set("test_remove", "0")
	res := fx.send(t, fx.reviewer, path, fields, nil)
	if res.status != http.StatusSeeOther {
		t.Fatalf("remove final optional draft test: HTTP %d", res.status)
	}
	kept, err := fx.app.Draft(fx.reviewer.actor, "pilot", draft.ID)
	page, status := response(t, fx.reviewer.client.Get, fx.visit+path)
	if err != nil || kept.Revision != draft.Revision+1 || len(kept.Baseline.Behaviors[0].Tests) != 0 ||
		status != http.StatusOK || !strings.Contains(page, "Add test") || strings.Contains(page, "Remove test") {
		t.Fatalf("empty saved test plan lost its editable behavior or offered a nonexistent removal: HTTP %d, %v", status, err)
	}
	view, status := response(t, fx.viewer.client.Get, fx.visit+path)
	if status != http.StatusOK || !strings.Contains(view, "Optional · 10000 basis points") || strings.Contains(view, "Add test") {
		t.Fatalf("Viewer form for a valid empty optional roster did not stay read-only: HTTP %d", status)
	}
	fields = draftForm(fx.reviewer.csrf, kept)
	fields.Set("intent", "add_test")
	if res := fx.send(t, fx.reviewer, path, fields, nil); res.status != http.StatusSeeOther {
		t.Fatalf("cannot resume editing optional test plan HTTP %d at %s", res.status, strconv.FormatUint(kept.Revision, 10))
	}
}
