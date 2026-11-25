package browser_test

import (
	"bytes"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
	"release-evidence-board/internal/browser"
)

func TestBrowserMalformedMutationsLeaveVerifiedHoldAndEvidenceIntact(t *testing.T) {
	fx := newAuthorityFixture(t)
	if err := fx.app.SuspendAssessments("reviewer", "pilot"); err != nil {
		t.Fatal(err)
	}
	const revalidate = "/p/pilot/health/revalidate"
	rejected := fx.send(t, fx.reviewer, revalidate, url.Values{"unexpected": {"on"}}, nil)
	health, status := response(t, fx.reviewer.client.Get, fx.visit+"/p/pilot/health")
	if rejected.status != http.StatusUnprocessableEntity || status != http.StatusOK ||
		!strings.Contains(health, "Assessment and report creation paused") || !strings.Contains(health, "Verify and revalidate") {
		t.Fatalf("extraneous revalidation field changed the held reviewer view: POST %d, health %d", rejected.status, status)
	}
	if _, status := response(t, fx.operator.client.Get, fx.visit+"/p/pilot/health"); status != http.StatusOK {
		t.Fatalf("assigned operator cannot inspect a current hold: %d", status)
	}
	candidate := "/p/pilot/candidates/" + fx.candidateID
	retained, _, _, err := fx.app.Candidate(fx.reviewer.actor, "pilot", fx.candidateID)
	if err != nil {
		t.Fatal(err)
	}
	stamp := board.FormatUTC(time.Now().UTC().Add(-time.Minute))
	upload := followupBrowserCSV(t, followupBrowserResult(retained, "R1", "BEH1", "P1", "T1", "AFTER-VERIFY", "HELD-RESULT", stamp, "PASS"))
	heldUpload := fx.send(t, fx.reviewer, candidate+"/preview", nil, upload)
	if heldUpload.status != http.StatusServiceUnavailable || strings.Contains(heldUpload.body, "Commit preview") {
		t.Fatalf("held valid evidence reached a fresh projection: HTTP %d", heldUpload.status)
	}
	accepted := fx.send(t, fx.reviewer, revalidate, nil, nil)
	if accepted.status != http.StatusSeeOther || accepted.location != "/p/pilot" || fx.state(t).held {
		t.Fatalf("correct revalidation did not clear verified hold: %+v", accepted)
	}
	validUpload := fx.send(t, fx.reviewer, candidate+"/preview", nil, upload)
	token := authorityPreviewToken.FindStringSubmatch(validUpload.body)
	if validUpload.status != http.StatusOK || len(token) != 2 {
		t.Fatalf("revalidated candidate could not preview the same valid file: HTTP %d", validUpload.status)
	}
	if canceled := fx.send(t, fx.reviewer, candidate+"/cancel", url.Values{"preview": {token[1]}}, nil); canceled.status != http.StatusSeeOther {
		t.Fatalf("valid staging control failed to cancel: HTTP %d", canceled.status)
	}
	before := fx.state(t)
	for _, path := range []string{"/commit", "/cancel"} {
		res := fx.send(t, fx.reviewer, candidate+path, url.Values{"preview": {"one", "two"}}, nil)
		if res.status != http.StatusUnprocessableEntity || res.location != "" {
			t.Fatalf("ambiguous %s reached a custody mutation: %+v", path, res)
		}
	}
	for _, fields := range []struct {
		name  string
		files []string
		extra bool
	}{
		{"no file", nil, false},
		{"wrong file field", []string{"attachment"}, false},
		{"multiple file fields", []string{"results", "attachment"}, false},
		{"multiple result files", []string{"results", "results"}, false},
		{"unexpected scalar field", []string{"results"}, true},
	} {
		t.Run(fields.name, func(t *testing.T) {
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			if err := form.WriteField("_csrf", fx.reviewer.csrf); err != nil {
				t.Fatal(err)
			}
			if fields.extra {
				if err := form.WriteField("unexpected", "on"); err != nil {
					t.Fatal(err)
				}
			}
			for _, key := range fields.files {
				file, err := form.CreateFormFile(key, "fixture.csv")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write([]byte("uncommitted synthetic upload")); err != nil {
					t.Fatal(err)
				}
			}
			if err := form.Close(); err != nil {
				t.Fatal(err)
			}
			page, status := response(t, func(endpoint string) (*http.Response, error) {
				return fx.reviewer.client.Post(endpoint, form.FormDataContentType(), &body)
			}, fx.visit+candidate+"/preview")
			if status != http.StatusUnprocessableEntity || !strings.Contains(page, "Choose one results CSV file.") ||
				strings.Contains(page, "Commit preview") {
				t.Fatalf("malformed upload exposed a commit at HTTP %d", status)
			}
		})
	}
	if after := fx.state(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("malformed import requests changed protected state: before=%+v after=%+v", before, after)
	}
}

func TestBrowserAbsentObjectsAndAssignedRolesDoNotExposeArtifacts(t *testing.T) {
	fx := newAuthorityFixture(t)
	for _, path := range []string{
		"/candidates/absent", "/candidates/absent/manifest", "/candidates/absent/import",
		"/candidates/absent/reports", "/candidates/absent/behaviors/BEH1",
		"/candidates/" + fx.candidateID + "/behaviors/absent",
		"/receipts/absent", "/receipts/absent/source", "/reports/absent?download=1", "/baselines/absent",
	} {
		reply, err := fx.reviewer.client.Get(fx.visit + "/p/pilot" + path)
		if err != nil {
			t.Fatal(err)
		}
		if err := reply.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if reply.StatusCode != http.StatusNotFound || reply.Header.Get("Content-Disposition") != "" {
			t.Errorf("absent object %s returned %d or supplied an attachment", path, reply.StatusCode)
		}
	}
	unknownCandidate := fx.send(t, fx.reviewer, "/p/pilot/candidates/absent/preview", nil, []byte("synthetic source"))
	if unknownCandidate.status != http.StatusNotFound || strings.Contains(unknownCandidate.body, "Commit preview") {
		t.Fatalf("absent candidate preview: %+v", unknownCandidate)
	}
	for _, request := range []struct {
		session authoritySession
		path    string
	}{
		{fx.viewer, "/candidates/" + fx.candidateID + "/import"},
		{fx.operator, "/baselines"},
		{fx.operator, "/candidates/" + fx.candidateID},
	} {
		page, status := response(t, request.session.client.Get, fx.visit+"/p/pilot"+request.path)
		if status != http.StatusForbidden || strings.Contains(page, "BEH1") || strings.Contains(page, "Download descriptor") {
			t.Fatalf("%s read %s outside assigned role: HTTP %d", request.session.actor, request.path, status)
		}
	}
	reply, err := fx.operator.client.Get(fx.visit + "/p/pilot")
	if err != nil {
		t.Fatal(err)
	}
	if err := reply.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if reply.StatusCode != http.StatusSeeOther || reply.Header.Get("Location") != "/p/pilot/health" {
		t.Fatalf("Operator-only workspace did not route to health: %s", reply.Status)
	}
	page, status := response(t, fx.viewer.client.Get, fx.visit+"/p/pilot/baselines")
	if status != http.StatusOK || !strings.Contains(page, "Download descriptor") || strings.Contains(page, "Draft revision") ||
		strings.Contains(page, "Create draft") {
		t.Fatalf("Viewer baseline catalog did not preserve read-only artifact actions: HTTP %d", status)
	}
}

func TestBrowserRootCanonicalProjectsRemovedAccountAndUnavailableStorage(t *testing.T) {
	for _, project := range []string{".", "..", "pilot"} {
		t.Run(project, func(t *testing.T) {
			app, err := board.Open(filepath.Join(t.TempDir(), "owned.db"), board.Settings{ProjectID: project, ProductID: "DEMO"})
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
			handler := browser.New(app, false)
			t.Cleanup(handler.Close)
			web := httptest.NewServer(handler)
			t.Cleanup(web.Close)

			canonical := "/p/" + project
			if project != "pilot" {
				canonical = "/p/~" + project
			}
			viewer := browserTestSession(t)
			checkBrowserRedirect(t, viewer, web.URL+"/", "/login")
			page, status := response(t, viewer.Get, web.URL+"/login")
			token := boundToken.FindStringSubmatch(page)
			if status != http.StatusOK || len(token) != 2 {
				t.Fatal("Viewer sign-in form is missing its session binding")
			}
			if _, status := post(t, viewer, web.URL+"/login", url.Values{
				"actor": {"viewer"}, "password": {browserPassword}, "_csrf": {token[1]},
			}); status != http.StatusSeeOther {
				t.Fatalf("canonical project did not allow its assigned Viewer: %d", status)
			}
			checkBrowserRedirect(t, viewer, web.URL+"/", canonical)
			page, status = response(t, viewer.Get, web.URL+canonical)
			if status != http.StatusOK || !strings.Contains(page, "Approve a behavior baseline") {
				t.Fatalf("valid canonical project has no reachable empty workspace: %d", status)
			}
			if project != "pilot" {
				return
			}
			if err := app.RemoveAccount("reviewer", "viewer"); err != nil {
				t.Fatal(err)
			}
			checkBrowserRedirect(t, viewer, web.URL+canonical, "/login")
			checkBrowserRedirect(t, viewer, web.URL+"/", "/login")

			reviewer, _ := loginSession(t, web.URL, "reviewer")
			for _, empty := range []struct{ route, marker string }{
				{"/baselines", "No drafts are saved."},
				{"/baselines", "No baseline has been approved"},
				{"/candidates/new", "Approve a baseline before you create a candidate."},
			} {
				body, status := response(t, reviewer.Get, web.URL+canonical+empty.route)
				if status != http.StatusOK || !strings.Contains(body, empty.marker) || strings.Contains(body, "name=\"build\"") {
					t.Fatalf("empty %s lost its recovery action or exposed release identity inputs: %d", empty.route, status)
				}
			}
			anonymous := browserTestSession(t)
			page, status = response(t, anonymous.Get, web.URL+"/login")
			token = boundToken.FindStringSubmatch(page)
			if status != http.StatusOK || len(token) != 2 {
				t.Fatal("anonymous sign-in form is missing its session binding")
			}
			if err := app.Close(); err != nil {
				t.Fatal(err)
			}
			if body, status := post(t, browserTestSession(t), web.URL+"/login", url.Values{
				"_csrf": {"unbound-token"}, "actor": {"reviewer"}, "password": {browserPassword},
			}); status != http.StatusForbidden || strings.Contains(body, browserPassword) {
				t.Fatalf("missing session binding did not fail closed while the rejection audit store was unavailable: HTTP %d", status)
			}
			for _, call := range []func() (string, int){
				func() (string, int) { return response(t, reviewer.Get, web.URL+canonical) },
				func() (string, int) {
					return post(t, anonymous, web.URL+"/login", url.Values{
						"actor": {"reviewer"}, "password": {browserPassword}, "_csrf": {token[1]},
					})
				},
			} {
				body, status := call()
				if status != http.StatusInternalServerError || !strings.Contains(body, "The operation could not finish.") ||
					strings.Contains(body, "owned.db") || strings.Contains(body, browserPassword) {
					t.Fatalf("real closed store did not fail safely: HTTP %d", status)
				}
			}
		})
	}
}

func TestBrowserClientDisconnectDoesNotPoisonNextResponse(t *testing.T) {
	app, err := board.Open(filepath.Join(t.TempDir(), "owned.db"), board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	handler := browser.New(app, false)
	t.Cleanup(handler.Close)
	entered, attempted := make(chan struct{}), make(chan bool, 1)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "disconnect=1" {
			handler.ServeHTTP(w, r)
			return
		}
		close(entered)
		select {
		case <-r.Context().Done():
			handler.ServeHTTP(w, r)
			attempted <- true
		case <-time.After(5 * time.Second):
			attempted <- false
		}
	}))
	t.Cleanup(web.Close)
	original := log.Writer()
	var capture bytes.Buffer
	log.SetOutput(io.MultiWriter(original, &capture))
	t.Cleanup(func() { log.SetOutput(original) })
	socket, err := net.DialTimeout("tcp", strings.TrimPrefix(web.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if _, err := io.WriteString(socket, "GET /style.css?disconnect=1 HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("owned HTTP connection did not enter the handler")
	}
	if err := socket.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	if err := socket.Close(); err != nil {
		t.Fatal(err)
	}
	if !<-attempted || !strings.Contains(capture.String(), "response transmission interrupted") {
		t.Fatalf("real canceled HTTP transport did not produce the scoped write observation: %q", capture.String())
	}
	css, status := response(t, web.Client().Get, web.URL+"/style.css")
	if status != http.StatusOK || !strings.Contains(css, ".status") {
		t.Fatalf("previous interrupted response poisoned the next client: HTTP %d", status)
	}
}

func browserTestSession(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func checkBrowserRedirect(t *testing.T, client *http.Client, endpoint, target string) {
	t.Helper()
	reply, err := client.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := reply.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if reply.StatusCode != http.StatusSeeOther || reply.Header.Get("Location") != target {
		t.Fatalf("%s returned %d to %q, want 303 to %q", endpoint, reply.StatusCode, reply.Header.Get("Location"), target)
	}
}
