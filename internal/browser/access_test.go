package browser_test

import (
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"release-evidence-board/internal/board"
	"release-evidence-board/internal/browser"
)

const browserPassword = "synthetic-browser-password"

func startTestBrowser(t *testing.T) (*board.Board, string, string) {
	t.Helper()
	app, err := board.Open(filepath.Join(t.TempDir(), "test.db"), board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := app.Bootstrap("reviewer", browserPassword); err != nil {
		t.Fatal(err)
	}
	if err := app.SetAccount("reviewer", "viewer", browserPassword, []board.Role{board.Viewer}); err != nil {
		t.Fatal(err)
	}
	draft, err := app.CreateDraft("reviewer", "pilot", board.Baseline{
		ProductID: "DEMO", ID: "B1", Revision: 1, Behaviors: []board.Behavior{{
			ID: "BEH1", Revision: 1, Required: true, Text: "An assigned user can read the record",
			Policy: board.Policy{ID: "P1", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
			Tests:  []board.TestPlan{{ID: "T1", ExpectedOutcome: "Authorized response", MustPass: true, PlannedAttempts: 1}},
		}},
	}, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := app.Approve("reviewer", "pilot", draft.ID, draft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	c, reused, err := app.CreateCandidate("reviewer", "pilot", board.CandidateInput{
		Manifest: board.Manifest{
			ProductID: "DEMO", ReleaseID: "R1", ApplicationVersion: "1.0", BuildDigest: strings.Repeat("a", 64),
			PromptDigest: "not_applicable", ModelConfigDigest: "not_applicable", KnowledgeDigest: "not_applicable", EnvironmentID: "E1",
		},
		BaselineDigest: approved.Descriptor.Digest,
	})
	if err != nil || reused {
		t.Fatalf("candidate fixture = %#v %v %v", c, reused, err)
	}
	handler := browser.New(app, false)
	t.Cleanup(handler.Close)
	web := httptest.NewServer(handler)
	t.Cleanup(web.Close)
	return app, web.URL, c.ID
}

var boundToken = regexp.MustCompile("name=\"_csrf\" value=\"([^\"]+)\"")

func loginSession(t *testing.T, visit, actor string) (*http.Client, string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	body, status := response(t, client.Get, visit+"/login")
	match := boundToken.FindStringSubmatch(body)
	if status != http.StatusOK || len(match) != 2 {
		t.Fatalf("login screen = %d %s", status, body)
	}
	_, status = post(t, client, visit+"/login", url.Values{"_csrf": {match[1]}, "actor": {actor}, "password": {browserPassword}})
	if status != http.StatusSeeOther {
		t.Fatalf("login = %d", status)
	}
	body, status = response(t, client.Get, visit+"/p/pilot")
	match = boundToken.FindStringSubmatch(body)
	if status != http.StatusOK || len(match) != 2 {
		t.Fatalf("workspace = %d %s", status, body)
	}
	return client, match[1]
}

func TestAC00102ViewerCannotSubmitProtectedMutations(t *testing.T) {
	app, visit, id := startTestBrowser(t)
	client, csrf := loginSession(t, visit, "viewer")
	path := "/p/pilot/candidates/" + id
	endpoints := []struct {
		path  string
		extra url.Values
	}{
		{"/p/pilot/drafts", url.Values{"baseline_id": {"Second"}}},
		{"/p/pilot/drafts/D-1", url.Values{"intent": {"add_behavior"}, "revision": {"1"}}},
		{"/p/pilot/drafts/D-1/approve", url.Values{"revision": {"1"}}},
		{"/p/pilot/candidates", url.Values{}},
		{path + "/preview", url.Values{}},
		{path + "/commit", url.Values{"preview": {"absent"}}},
		{path + "/cancel", url.Values{"preview": {"absent"}}},
		{path + "/reports", url.Values{}},
		{"/p/pilot/health/revalidate", url.Values{}},
	}
	for _, attempt := range endpoints {
		t.Run(attempt.path, func(t *testing.T) {
			attempt.extra.Set("_csrf", csrf)
			_, status := post(t, client, visit+attempt.path, attempt.extra)
			if status != http.StatusForbidden {
				t.Fatalf("viewer mutation status = %d, want forbidden", status)
			}
		})
	}
	assessment, err := app.Assess("viewer", "pilot", id)
	if err != nil || assessment.Status != board.Blocked || assessment.LedgerHighWater != 0 {
		t.Fatalf("denied mutations changed evidence: %#v %v", assessment, err)
	}
}

func TestAC01305ImportRequestLimitIncludesPreviewAndCommit(t *testing.T) {
	_, visit, id := startTestBrowser(t)
	client, csrf := loginSession(t, visit, "reviewer")
	path := fmt.Sprintf("%s/p/pilot/candidates/%s", visit, id)
	for i := 0; i < 10; i++ {
		if i%2 == 0 {
			_, status := post(t, client, path+"/preview", url.Values{"_csrf": {csrf}})
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("preview %d = %d", i+1, status)
			}
		} else {
			_, status := post(t, client, path+"/commit", url.Values{"_csrf": {csrf}, "preview": {"otherwise-unbound"}})
			if status != http.StatusNotFound {
				t.Fatalf("commit %d = %d", i+1, status)
			}
		}
	}
	_, status := post(t, client, path+"/preview", url.Values{"_csrf": {csrf}})
	if status != http.StatusTooManyRequests {
		t.Fatalf("eleventh import endpoint request = %d, want 429", status)
	}
}

func TestSPEC9ImmutableCandidateIdentityVisible(t *testing.T) {
	_, visit, id := startTestBrowser(t)
	client, _ := loginSession(t, visit, "reviewer")
	for _, route := range []string{"", "/behaviors/BEH1", "/import", "/reports"} {
		t.Run(route, func(t *testing.T) {
			body, status := response(t, client.Get, visit+"/p/pilot/candidates/"+id+route)
			displayed := strings.SplitN(body, "<details", 2)[0]
			if status != http.StatusOK || !strings.Contains(displayed, "<code>"+id+"</code>") {
				t.Fatalf("immutable candidate ID is absent from the visible %q screen", route)
			}
		})
	}
}

func TestSessionCleanupConstructionAndClose(t *testing.T) {
	app, _, _ := startTestBrowser(t)
	for range 20 {
		handler := browser.New(app, false)
		var closes sync.WaitGroup
		closes.Go(handler.Close)
		closes.Go(handler.Close)
		closes.Wait()
	}
}
