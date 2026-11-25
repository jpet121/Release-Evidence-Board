package browser_test

import (
	"encoding/hex"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"release-evidence-board/internal/board"
	"release-evidence-board/internal/browser"
)

func TestAC01304BrowserSessionAndCSRF(t *testing.T) {
	app, err := board.Open(filepath.Join(t.TempDir(), "test.db"), board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := app.Bootstrap("reviewer", "synthetic-test-password"); err != nil {
		t.Fatal(err)
	}
	server := browser.New(app, false)
	defer server.Close()
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	body, status := response(t, client.Get, httpServer.URL+"/p/pilot")
	if status != http.StatusSeeOther || strings.Contains(body, "Active candidates") {
		t.Fatalf("anonymous project = %d %s", status, body)
	}
	body, status = response(t, client.Get, httpServer.URL+"/login")
	if status != http.StatusOK {
		t.Fatalf("login GET = %d", status)
	}
	token := csrfValue(t, body)
	credentials := url.Values{"actor": {"reviewer"}, "password": {"synthetic-test-password"}}
	_, status = post(t, client, httpServer.URL+"/login", credentials)
	if status != http.StatusForbidden {
		t.Fatalf("unbound login POST = %d", status)
	}
	credentials.Set("_csrf", token)
	_, status = post(t, client, httpServer.URL+"/login", credentials)
	if status != http.StatusSeeOther {
		t.Fatalf("authenticated login POST = %d", status)
	}
	body, status = response(t, client.Get, httpServer.URL+"/p/pilot")
	if status != http.StatusOK || !strings.Contains(body, "NOT_ASSESSABLE") {
		t.Fatalf("empty authorized workspace = %d %s", status, body)
	}
	if current := csrfValue(t, body); current == token {
		t.Fatal("successful login did not rotate the synchronizer token")
	}
	_, status = post(t, client, httpServer.URL+"/logout", url.Values{"_csrf": {token}})
	if status != http.StatusForbidden {
		t.Fatalf("old synchronizer token accepted after login: %d", status)
	}
	body, status = response(t, client.Get, httpServer.URL+"/p/unassigned")
	if status != http.StatusNotFound || strings.Contains(body, "Active candidates") {
		t.Fatalf("foreign request exposed workspace: %d %s", status, body)
	}
}

func csrfValue(t *testing.T, body string) string {
	t.Helper()
	token := regexp.MustCompile(`name="_csrf" value="([^"]+)"`).FindStringSubmatch(body)
	if len(token) != 2 {
		t.Fatal("form lacks synchronizer token")
	}
	raw, err := hex.DecodeString(token[1])
	if err != nil || len(raw) != 32 {
		t.Fatal("synchronizer token is not 32 hex-encoded bytes")
	}
	return token[1]
}

func post(t *testing.T, client *http.Client, url string, values url.Values) (string, int) {
	t.Helper()
	return response(t, func(path string) (*http.Response, error) { return client.PostForm(path, values) }, url)
}

func response(t *testing.T, request func(string) (*http.Response, error), url string) (string, int) {
	t.Helper()
	res, err := request(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body), res.StatusCode
}
