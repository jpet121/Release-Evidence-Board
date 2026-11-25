package browser_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"release-evidence-board/internal/board"
)

func TestRF07LiveAndPausedCandidateViews(t *testing.T) {
	_, visit, id := startTestBrowser(t)
	client, _ := loginSession(t, visit, "reviewer")
	for _, route := range []string{"/p/pilot/candidates/" + id, "/p/pilot/candidates/" + id + "/behaviors/BEH1"} {
		active, status := response(t, client.Get, visit+route)
		if status != http.StatusOK || !strings.Contains(active, "http-equiv=\"refresh\" content=\"59\"") {
			t.Fatalf("view %s lacks the 59-second refresh interval: %d", route, status)
		}
		historical, status := response(t, client.Get, visit+route+"?refresh=off")
		if status != http.StatusOK || strings.Contains(historical, "http-equiv=\"refresh\"") ||
			!strings.Contains(historical, "Historical display · automatic refresh is paused") {
			t.Fatalf("view %s does not identify paused result: %d", route, status)
		}
	}
}

func TestRF09RetainedPagesWarnWhileFrozenBytesStayIdentical(t *testing.T) {
	app, visit, id := startTestBrowser(t)
	client, _ := loginSession(t, visit, "reviewer")
	record, err := app.Freeze("reviewer", "pilot", id)
	if err != nil {
		t.Fatal(err)
	}
	_, original, err := app.ReportHTML("reviewer", "pilot", record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.SuspendAssessments("reviewer", "pilot"); err != nil {
		t.Fatal(err)
	}
	route := "/p/pilot/candidates/" + id + "/reports"
	view, status := response(t, client.Get, visit+route)
	if status != http.StatusOK || !strings.Contains(view, "Evidence integrity warning") {
		t.Fatalf("historical report history lacked warning: %d", status)
	}
	frozen, err := client.Get(visit + "/p/pilot/reports/" + record.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, readError := io.ReadAll(frozen.Body)
	closeError := frozen.Body.Close()
	if readError != nil || closeError != nil || frozen.Header.Get("X-REB-Integrity-Warning") != "assessment-suspended" || !bytes.Equal(raw, original) {
		t.Fatal("frozen HTML changed or its HTTP integrity warning is missing")
	}
}

func TestSTD04MalformedBehaviorEditDoesNotChangeDraft(t *testing.T) {
	app, visit, id := startTestBrowser(t)
	client, csrf := loginSession(t, visit, "reviewer")
	candidate, _, approved, err := app.Candidate("reviewer", "pilot", id)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := app.CreateDraft("reviewer", "pilot", approved.Baseline, board.DraftOptions{FromBaseline: candidate.BaselineDigest})
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		key    string
		values []string
	}{
		{"required", []string{"yes"}}, {"must_pass_0", []string{"false"}},
		{"text", []string{"first", "second"}}, {"unrecognized", []string{"ignored"}},
	} {
		t.Run(check.key, func(t *testing.T) {
			values := draftForm(csrf, draft)
			values[check.key] = check.values
			_, status := post(t, client, visit+"/p/pilot/drafts/"+draft.ID, values)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("invalid field %q accepted with %d", check.key, status)
			}
			got, err := app.Draft("reviewer", "pilot", draft.ID)
			if err != nil || got.Revision != draft.Revision || !got.Baseline.Behaviors[0].Required || !got.Baseline.Behaviors[0].Tests[0].MustPass {
				t.Fatalf("malformed edit changed the draft: revision %d, %v", got.Revision, err)
			}
		})
	}
}

func TestC01FullSizedExpectedOutcomesReachHTTPAuthoring(t *testing.T) {
	app, visit, _ := startTestBrowser(t)
	client, csrf := loginSession(t, visit, "reviewer")
	const plannedTests = 1000
	maximumExpected := strings.Repeat("😀", 2000)
	tests := make([]board.TestPlan, plannedTests)
	for i := range tests {
		tests[i] = board.TestPlan{ID: fmt.Sprintf("T%04d", i), ExpectedOutcome: maximumExpected, MustPass: true, PlannedAttempts: 1}
	}
	draft, err := app.CreateDraft("reviewer", "pilot", board.Baseline{ProductID: "DEMO", ID: "large", Revision: 1,
		Behaviors: []board.Behavior{{ID: "largeBehavior", Revision: 1, Required: true, Text: "Initial",
			Policy: board.Policy{ID: "largePolicy", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24}, Tests: tests}},
	}, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	values := draftForm(csrf, draft)
	values.Set("text", "Updated")
	_, status := post(t, client, visit+"/p/pilot/drafts/"+draft.ID, values)
	if status != http.StatusSeeOther {
		t.Fatalf("valid large authoring form was rejected with HTTP %d", status)
	}
	stored, err := app.Draft("reviewer", "pilot", draft.ID)
	if err != nil || stored.Baseline.Behaviors[0].Text != "Updated" || len(stored.Baseline.Behaviors[0].Tests) != plannedTests ||
		stored.Baseline.Behaviors[0].Tests[plannedTests-1].ExpectedOutcome != maximumExpected {
		t.Fatalf("large roster edit lost text: %v", err)
	}
}

func draftForm(csrf string, draft board.Draft) url.Values {
	behavior := draft.Baseline.Behaviors[0]
	values := url.Values{"_csrf": {csrf}, "revision": {strconv.FormatUint(draft.Revision, 10)}, "behavior_index": {"0"},
		"behavior_id": {behavior.ID}, "intent": {"save_behavior"}, "text": {behavior.Text},
		"policy_id": {behavior.Policy.ID}, "minimum": {strconv.Itoa(behavior.Policy.MinPassBasisPoints)},
		"max_age": {strconv.Itoa(behavior.Policy.MaxAgeHours)}}
	if behavior.Required {
		values.Set("required", "1")
	}
	for i, row := range behavior.Tests {
		key := strconv.Itoa(i)
		values.Set("test_"+key, row.ID)
		values.Set("expected_"+key, row.ExpectedOutcome)
		values.Set("attempts_"+key, strconv.Itoa(row.PlannedAttempts))
		if row.MustPass {
			values.Set("must_pass_"+key, "1")
		}
	}
	return values
}
