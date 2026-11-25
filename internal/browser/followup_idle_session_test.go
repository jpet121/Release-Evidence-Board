package browser_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"release-evidence-board/internal/board"
	"release-evidence-board/internal/browser"
)

func TestFollowupActualSessionExpiresAtIdleBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		app, err := board.Open(filepath.Join(t.TempDir(), "sessions.db"), board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
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
		handler := browser.New(app, false)
		t.Cleanup(handler.Close)
		first := establishIdleSession(t, handler)
		second := establishIdleSession(t, handler)
		if first.Value == second.Value {
			t.Fatal("fixture must have distinct sessions")
		}

		time.Sleep(30*time.Minute - time.Nanosecond)
		before := httptest.NewRequest(http.MethodGet, "/p/pilot", nil)
		before.AddCookie(first)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, before)
		if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), "reviewer") {
			t.Fatalf("valid idle session stopped just before the boundary: HTTP %d", result.Code)
		}

		time.Sleep(time.Nanosecond)
		at := httptest.NewRequest(http.MethodGet, "/p/pilot", nil)
		at.AddCookie(second)
		result = httptest.NewRecorder()
		handler.ServeHTTP(result, at)
		if result.Code != http.StatusSeeOther || result.Header().Get("Location") != "/login" {
			t.Fatalf("proof of reauthentication missing at 30 minutes of true inactivity: HTTP %d", result.Code)
		}
		recordThresholdHTTPInstance(t, "AC-001-05.P001", map[string]any{"inactiveSeconds": 1800}, "Reauthentication required")
	})
}

func establishIdleSession(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/login", nil)
	loginPage := httptest.NewRecorder()
	handler.ServeHTTP(loginPage, request)
	csrf := boundToken.FindStringSubmatch(loginPage.Body.String())
	if loginPage.Code != http.StatusOK || len(csrf) != 2 {
		t.Fatalf("could not acquire a real login form: HTTP %d", loginPage.Code)
	}
	sessionCookie := observedSessionCookie(t, loginPage)
	form := url.Values{"actor": {"reviewer"}, "password": {browserPassword}, "_csrf": {csrf[1]}}
	request = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(sessionCookie)
	authenticated := httptest.NewRecorder()
	handler.ServeHTTP(authenticated, request)
	if authenticated.Code != http.StatusSeeOther || authenticated.Header().Get("Location") != "/p/pilot" {
		t.Fatalf("could not establish actual reviewer cookie: HTTP %d", authenticated.Code)
	}
	return observedSessionCookie(t, authenticated)
}

func observedSessionCookie(t *testing.T, result *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	response := result.Result()
	defer response.Body.Close()
	for _, cookie := range response.Cookies() {
		if cookie.Name == "reb_session" && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatal("real handler did not issue a session cookie")
	return nil
}
