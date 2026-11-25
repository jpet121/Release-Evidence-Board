package browser_test

import (
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"release-evidence-board/internal/board"
)

func TestBehaviorLinksAcceptPeriodOnlyIdentifiers(t *testing.T) {
	app, visit, _ := startTestBrowser(t)
	client, _ := loginSession(t, visit, "reviewer")
	roster := board.Baseline{ProductID: "DEMO", ID: "periods", Revision: 1}
	for _, spec := range []struct{ id, test, text string }{
		{".", "one-dot", "Single period behavior"}, {"..", "two-dots", "Double period behavior"},
	} {
		roster.Behaviors = append(roster.Behaviors, board.Behavior{ID: spec.id, Revision: 1, Required: true, Text: spec.text,
			Policy: board.Policy{ID: "same-policy", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
			Tests:  []board.TestPlan{{ID: spec.test, ExpectedOutcome: "Pass", MustPass: true, PlannedAttempts: 1}},
		})
	}
	draft, err := app.CreateDraft("reviewer", "pilot", roster, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := app.Approve("reviewer", "pilot", draft.ID, draft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	candidate, reused, err := app.CreateCandidate("reviewer", "pilot", board.CandidateInput{
		Manifest: board.Manifest{ProductID: "DEMO", ReleaseID: "period-examples", ApplicationVersion: "1.1", BuildDigest: strings.Repeat("c", 64),
			PromptDigest: "not_applicable", ModelConfigDigest: "not_applicable", KnowledgeDigest: "not_applicable", EnvironmentID: "E2"},
		BaselineDigest: baseline.Descriptor.Digest,
	})
	if err != nil || reused {
		t.Fatalf("candidate err=%v reused=%t", err, reused)
	}
	contents, status := response(t, client.Get, visit+"/p/pilot/candidates/"+candidate.ID)
	if status != http.StatusOK {
		t.Fatalf("board status %d", status)
	}
	for _, behavior := range roster.Behaviors {
		t.Run(behavior.ID, func(t *testing.T) {
			row := regexp.MustCompile("href=\"([^\"]+)\" class=\"row-link\">" + regexp.QuoteMeta(behavior.ID+" · "+behavior.Text))
			target := row.FindStringSubmatch(contents)
			if len(target) != 2 {
				t.Fatal("behavior lacks a followable link")
			}
			detail, status := response(t, client.Get, visit+html.UnescapeString(target[1]))
			if status != http.StatusOK || !strings.Contains(detail, "<h2>"+behavior.Text+"</h2>") {
				t.Fatalf("following rendered link produced %d instead of exact behavior detail", status)
			}
		})
	}
}
