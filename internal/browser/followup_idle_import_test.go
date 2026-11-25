package browser_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"release-evidence-board/internal/board"
	"release-evidence-board/internal/browser"
)

func TestFollowupExpiredSessionCannotStageOrCommitEvidence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, err := board.Open(filepath.Join(t.TempDir(), "import-session.db"), board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
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
		draft, err := app.CreateDraft("reviewer", "pilot", board.Baseline{
			ID: "B1", Revision: 1, ProductID: "DEMO", Behaviors: []board.Behavior{{
				ID: "BEH1", Revision: 1, Required: true, Text: "Protected import outcome",
				Policy: board.Policy{ID: "P1", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
				Tests:  []board.TestPlan{{ID: "T1", ExpectedOutcome: "Exactly one authentic outcome", MustPass: true, PlannedAttempts: 1}},
			}},
		}, board.DraftOptions{})
		if err != nil {
			t.Fatal(err)
		}
		approval, err := app.Approve("reviewer", "pilot", draft.ID, draft.Revision)
		if err != nil {
			t.Fatal(err)
		}
		candidate, reused, err := app.CreateCandidate("reviewer", "pilot", board.CandidateInput{
			BaselineDigest: approval.Descriptor.Digest,
			Manifest: board.Manifest{ProductID: "DEMO", ReleaseID: "R1", ApplicationVersion: "1.0",
				BuildDigest: strings.Repeat("a", 64), PromptDigest: "not_applicable", ModelConfigDigest: "not_applicable",
				KnowledgeDigest: "not_applicable", EnvironmentID: "E1"},
		})
		if err != nil || reused {
			t.Fatalf("protected candidate fixture was not created: %v", err)
		}
		handler := browser.New(app, false)
		t.Cleanup(handler.Close)
		previewCookie, previewCSRF := sessionWithCurrentCSRF(t, handler)
		commitCookie, commitCSRF := sessionWithCurrentCSRF(t, handler)
		time.Sleep(25 * time.Minute)
		stamp := board.FormatUTC(time.Now().UTC().Add(-time.Minute))
		raw := followupBrowserCSV(t, followupBrowserResult(candidate, "R1", "BEH1", "P1", "T1", "IDLE-PASS", "IDLE-PASS-RESULT", stamp, "PASS"))
		staged, err := app.PreviewImport("reviewer", "pilot", candidate.ID, raw)
		if err != nil || staged.Token == "" || len(staged.Issues) != 0 {
			t.Fatalf("valid preview fixture could not be independently staged: %v", err)
		}
		before, err := app.Audit("reviewer", "pilot")
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Minute) // Both sessions expire. The preview is still within its own 30-minute lifetime.
		endpoint := "/p/pilot/candidates/" + candidate.ID
		for _, item := range []struct {
			name string
			req  *http.Request
		}{
			{"preview", idlePreviewRequest(t, endpoint+"/preview", previewCookie, previewCSRF, raw)},
			{"commit", idleCommitRequest(endpoint+"/commit", commitCookie, commitCSRF, staged.Token)},
		} {
			reply := httptest.NewRecorder()
			handler.ServeHTTP(reply, item.req)
			if reply.Code != http.StatusSeeOther || reply.Header().Get("Location") != "/login" {
				t.Errorf("expired valid session reached protected %s: HTTP %d location %q", item.name, reply.Code, reply.Header().Get("Location"))
			}
		}
		after, err := app.Audit("reviewer", "pilot")
		if err != nil || len(after) < len(before) || slices.ContainsFunc(after[len(before):], func(event board.AuditEvent) bool {
			return event.Outcome == "success" && (event.Action == "import.preview" || event.Action == "import.commit")
		}) {
			t.Fatalf("expired request created successful staging or commit evidence: %v", err)
		}
		state, err := app.Assess("reviewer", "pilot", candidate.ID)
		if err != nil || state.Status != board.Blocked || state.LedgerHighWater != 0 {
			t.Fatalf("expired request changed the evidence ledger: %v", err)
		}

		fresh, csrf := sessionWithCurrentCSRF(t, handler)
		reply := httptest.NewRecorder()
		handler.ServeHTTP(reply, idleCommitRequest(endpoint+"/commit", fresh, csrf, staged.Token))
		if reply.Code != http.StatusSeeOther || !strings.HasPrefix(reply.Header().Get("Location"), "/p/pilot/receipts/") {
			t.Fatalf("fresh valid session could not commit the same unexpired preview: HTTP %d", reply.Code)
		}
		state, err = app.Assess("reviewer", "pilot", candidate.ID)
		if err != nil || state.Status != board.Sufficient || state.LedgerHighWater != 1 ||
			len(state.Behaviors) != 1 || state.Behaviors[0].CurrentRunID != "IDLE-PASS" {
			t.Fatalf("fresh control did not commit the exact valid evidence once: %v", err)
		}
		t.Log("expired preview and commit denied; fresh session committed the same valid unexpired preview")
	})
}

func sessionWithCurrentCSRF(t *testing.T, handler http.Handler) (*http.Cookie, string) {
	t.Helper()
	authenticated := establishIdleSession(t, handler)
	request := httptest.NewRequest(http.MethodGet, "/p/pilot", nil)
	request.AddCookie(authenticated)
	workspace := httptest.NewRecorder()
	handler.ServeHTTP(workspace, request)
	actual := boundToken.FindStringSubmatch(workspace.Body.String())
	if workspace.Code != http.StatusOK || len(actual) != 2 {
		t.Fatal("signed-in actor did not receive the actual protected form token")
	}
	return authenticated, actual[1]
}

func idlePreviewRequest(t *testing.T, path string, cookie *http.Cookie, csrf string, raw []byte) *http.Request {
	t.Helper()
	var content bytes.Buffer
	writer := multipart.NewWriter(&content)
	if err := writer.WriteField("_csrf", csrf); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFormFile("results", "idle-session-results.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, &content)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.AddCookie(cookie)
	return request
}

func idleCommitRequest(path string, cookie *http.Cookie, csrf, token string) *http.Request {
	form := url.Values{"_csrf": {csrf}, "preview": {token}}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	return request
}
