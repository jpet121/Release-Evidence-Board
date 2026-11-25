package browser_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

// SPEC-9 and AC-008. Check each grouped presentation label in a real browser
// route response. Two labels can share a glyph without sharing a decision.
func TestFollowupBrowserEachGroupedStatusKeepsItsOwnVisibleLabel(t *testing.T) {
	app, baseURL, candidateID := startTestBrowser(t)
	viewer, _ := loginSession(t, baseURL, "viewer")
	candidate, _, _, err := app.Candidate("reviewer", "pilot", candidateID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	passedAt := board.FormatUTC(now.Add(-5 * time.Minute))
	failedAt := board.FormatUTC(now.Add(-4 * time.Minute))
	path := baseURL + "/p/pilot/candidates/" + candidateID
	check := func(expected ...string) {
		t.Helper()
		body, status := response(t, viewer.Get, path)
		if status != http.StatusOK {
			t.Fatalf("candidate view returned HTTP %d", status)
		}
		for _, item := range expected {
			if !strings.Contains(body, item) {
				t.Errorf("candidate view lacks %s", item)
			}
		}
	}
	check(`class="status BLOCKED"><span aria-hidden="true">×</span> BLOCKED`,
		`class="status UNKNOWN"><span aria-hidden="true">?</span> UNKNOWN`)
	pass := followupBrowserResult(candidate, "R1", "BEH1", "P1", "T1", "PASSING", "PASS-1", passedAt, "PASS")
	commitFollowupBrowserCSV(t, app, candidateID, followupBrowserCSV(t, pass))
	check(`class="status SUFFICIENT"><span aria-hidden="true">✓</span> SUFFICIENT`,
		`class="status PASS"><span aria-hidden="true">✓</span> PASS`)
	fail := followupBrowserResult(candidate, "R1", "BEH1", "P1", "T1", "FAILING", "FAIL-1", failedAt, "FAIL")
	commitFollowupBrowserCSV(t, app, candidateID, followupBrowserCSV(t, fail))
	check(`class="status BLOCKED"><span aria-hidden="true">×</span> BLOCKED`,
		`class="status FAIL"><span aria-hidden="true">×</span> FAIL`)
	t.Log("SPEC-9 visible paired labels individually observed: PASS SUFFICIENT FAIL BLOCKED; later decisive failure was BLOCKED")
}
