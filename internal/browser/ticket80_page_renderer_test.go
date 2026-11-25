package browser

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTicket80PageRendererFailureNeverLeaksBufferedHTML(t *testing.T) {
	original := pages
	t.Cleanup(func() { pages = original })
	broken, err := template.New("pages").Parse(`{{define "head"}}PRIVATE-BUFFERED-LAYOUT{{end}}{{define "login"}}{{template "missing-layout" .}}{{end}}{{define "foot"}}end{{end}}`)
	if err != nil {
		t.Fatal(err)
	}
	server := New(newSessionBoard(t), false)
	t.Cleanup(server.Close)
	pages = broken
	// Run the public handler synchronously so template composition and its
	// restoration cannot race an HTTP server goroutine.
	failed := httptest.NewRecorder()
	server.ServeHTTP(failed, httptest.NewRequest(http.MethodGet, "http://example.test/login", nil))
	if failed.Code != http.StatusInternalServerError || failed.Header().Get("Location") != "" ||
		failed.Body.String() != "The board could not render this screen.\n" || strings.Contains(failed.Body.String(), "PRIVATE-BUFFERED-LAYOUT") {
		t.Fatal("actual template failure leaked buffered markup or appeared successful")
	}
	pages = original
	recovered := httptest.NewRecorder()
	server.ServeHTTP(recovered, httptest.NewRequest(http.MethodGet, "http://example.test/login", nil))
	if recovered.Code != http.StatusOK || !strings.Contains(recovered.Body.String(), "Sign in") {
		t.Fatal("page did not recover after actual renderer restoration")
	}
}
