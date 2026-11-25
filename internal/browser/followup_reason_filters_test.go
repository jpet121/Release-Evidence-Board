package browser_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupAllAvailableReasonFiltersPreserveRequiredDenominator(t *testing.T) {
	app, visit, _ := startTestBrowser(t)
	viewer, _ := loginSession(t, visit, "viewer")
	roster := board.Baseline{ProductID: "DEMO", ID: "REASONS", Revision: 1}
	for _, suffix := range []string{"NONE", "GAP", "ERROR", "SKIP", "UNKNOWN", "STALE", "FAIL", "SKEW"} {
		roster.Behaviors = append(roster.Behaviors, board.Behavior{
			ID: "B-" + suffix, Revision: 1, Required: true, Text: "Review reason " + suffix,
			Policy: board.Policy{ID: "POL-" + suffix, Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 1},
			Tests:  []board.TestPlan{{ID: "TEST-" + suffix, ExpectedOutcome: "Independently observed", MustPass: true, PlannedAttempts: 1}},
		})
	}
	draft, err := app.CreateDraft("reviewer", "pilot", roster, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := app.Approve("reviewer", "pilot", draft.ID, draft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	manifest := board.Manifest{ProductID: "DEMO", ReleaseID: "FILTER", ApplicationVersion: "1.0", BuildDigest: strings.Repeat("c", 64),
		PromptDigest: "not_applicable", ModelConfigDigest: "not_applicable", KnowledgeDigest: "not_applicable", EnvironmentID: "REASONS"}
	candidate, reused, err := app.CreateCandidate("reviewer", "pilot", board.CandidateInput{Manifest: manifest, BaselineDigest: approved.Descriptor.Digest})
	if err != nil || reused {
		t.Fatalf("could not create the required reason filter fixture: %v", err)
	}
	at := time.Now().UTC()
	var rows [][]string
	for _, observation := range []struct {
		suffix, outcome string
		time            time.Time
	}{
		{"GAP", "", at}, {"ERROR", "ERROR", at}, {"SKIP", "SKIPPED", at}, {"UNKNOWN", "UNKNOWN", at},
		{"STALE", "PASS", at.Add(-2 * time.Hour)}, {"FAIL", "FAIL", at}, {"SKEW", "PASS", at.Add(2 * time.Minute)},
	} {
		tag := observation.suffix
		row := followupBrowserResult(candidate, "FILTER", "B-"+tag, "POL-"+tag, "TEST-"+tag, "RUN-"+tag, "ROW-"+tag, board.FormatUTC(observation.time), observation.outcome)
		if observation.outcome == "" {
			row[1], row[2], row[11], row[13], row[14] = "RUN_ONLY", "", "", "", ""
		}
		rows = append(rows, row)
	}
	commitFollowupBrowserCSV(t, app, candidate.ID, followupBrowserCSV(t, rows...))
	for _, expected := range []struct {
		reason, visible string
	}{
		{"MISSING_RUN", "B-NONE"}, {"MISSING_SLOT", "B-GAP"}, {"ERROR_RESULT", "B-ERROR"},
		{"SKIPPED_RESULT", "B-SKIP"}, {"UNKNOWN_RESULT", "B-UNKNOWN"}, {"STALE_CURRENT", "B-STALE"},
		{"MUST_PASS_FAILED", "B-FAIL"}, {"RATE_BELOW_THRESHOLD", "B-FAIL"}, {"CLOCK_SKEW", "B-SKEW"},
	} {
		t.Run(expected.reason, func(t *testing.T) {
			path := visit + "/p/pilot/candidates/" + candidate.ID + "?" + url.Values{"reason": {expected.reason}}.Encode()
			body, status := response(t, viewer.Get, path)
			if status != http.StatusOK || !strings.Contains(body, "<option selected>"+expected.reason+"</option>") {
				t.Fatalf("browser did not retain selected reason %s: HTTP %d", expected.reason, status)
			}
			assertBlockedReasonCounts(t, body)
			for _, behavior := range roster.Behaviors {
				if strings.Contains(body, `id="behavior-`+behavior.ID+`"`) != (expected.visible == behavior.ID) {
					t.Fatalf("filter %s incorrectly exposed or hid %s", expected.reason, behavior.ID)
				}
			}
			t.Logf("reason=%s visible=%q required_counts=1,1,1,5 aggregate=BLOCKED", expected.reason, expected.visible)
		})
	}
	t.Run("unoffered-preview-reason-remains-blocked", func(t *testing.T) {
		path := visit + "/p/pilot/candidates/" + candidate.ID + "?reason=HISTORICAL_MISMATCH"
		body, status := response(t, viewer.Get, path)
		if status != http.StatusOK {
			t.Fatalf("unsupported direct query returned HTTP %d", status)
		}
		_, menuStart, found := strings.Cut(body, "<select name=\"reason\">")
		menu, _, closed := strings.Cut(menuStart, "</select>")
		if !found || !closed || strings.Count(menu, "<option") != 10 || strings.Contains(menu, "HISTORICAL_MISMATCH") {
			t.Fatal("live filter inventory does not show the nine actual reason options and the All reasons control")
		}
		assertBlockedReasonCounts(t, body)
		visible := strings.Count(body, `id="behavior-`)
		t.Logf("advisory unsupported query: filter_not_offered=true visible_rows=%d blocked_aggregate=true; unsupported display behavior is outside AC-009-03", visible)
	})
}

func assertBlockedReasonCounts(t *testing.T, body string) {
	t.Helper()
	summary, _, found := strings.Cut(body, "</section>")
	if !found || !strings.Contains(summary, `class="status BLOCKED"`) || strings.Contains(summary, `class="status SUFFICIENT"`) {
		t.Fatal("filtered evidence changed the aggregate BLOCKED header")
	}
	for _, count := range []string{"PASS 1", "FAIL 1", "STALE 1", "UNKNOWN 5"} {
		if !strings.Contains(summary, count) {
			t.Fatalf("filtered evidence changed the required roster count %s", count)
		}
	}
}
