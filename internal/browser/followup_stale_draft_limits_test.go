package browser_test

import (
	"bufio"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestBrowserStaleDraftPreservesSupportedRosterBounds(t *testing.T) {
	fx := newAuthorityFixture(t)
	for _, count := range []int{0, board.MaxAttempts} {
		t.Run(strconv.Itoa(count)+" tests", func(t *testing.T) {
			roster := make([]board.TestPlan, count)
			for i := range roster {
				roster[i] = board.TestPlan{ID: "TEST-" + strconv.Itoa(i+1), ExpectedOutcome: "Recorded outcome", PlannedAttempts: 1, MustPass: true}
			}
			draft, err := fx.app.CreateDraft("reviewer", "pilot", board.Baseline{
				ID: "LIMIT-" + strconv.Itoa(count), Revision: 1, ProductID: "DEMO",
				Behaviors: []board.Behavior{{
					ID: "LIMIT-BEHAVIOR", Revision: 1, Text: "Original editable behavior", Required: count > 0,
					Policy: board.Policy{ID: "LIMIT-POLICY", Revision: 1, MaxAgeHours: 24}, Tests: roster,
				}},
			}, board.DraftOptions{})
			if err != nil {
				t.Fatal(err)
			}
			path := "/p/pilot/drafts/" + draft.ID
			page, status := response(t, fx.contributor.client.Get, fx.visit+path)
			if status != http.StatusOK || !strings.Contains(page, "Original editable behavior") ||
				strings.Count(page, "<legend>Test TEST-") != count {
				t.Fatalf("contributor could not load the full saved roster at its supported limit: HTTP %d", status)
			}
			saved := draftForm(fx.reviewer.csrf, draft)
			saved.Set("text", "The newer saved behavior")
			if count > 0 {
				saved.Set("test_remove", "0")
				over := maps.Clone(saved)
				over.Set("extra", "beyond the maximum wire fields")
				reply := fx.send(t, fx.reviewer, path, over, nil)
				if reply.status != http.StatusBadRequest || loadBrowserDraft(t, fx.app, draft.ID).Revision != draft.Revision {
					t.Fatalf("one field beyond the structural maximum was not refused at parsing: HTTP %d", reply.status)
				}
			}
			submitRedirect(t, fx.reviewer.client, fx.visit+path, fx.reviewer.csrf, saved)
			stale := draftForm(fx.contributor.csrf, draft)
			if count > 0 {
				stale.Set("test_remove", "0")
			}
			stale.Set("text", "My unsaved notes")
			reply := fx.send(t, fx.contributor, path, stale, nil)
			if reply.status != http.StatusConflict || !strings.Contains(reply.body, "My unsaved notes") ||
				strings.Count(reply.body, "<h3>Test TEST-") != count {
				t.Fatalf("stale recovery lost part of the supported roster: HTTP %d", reply.status)
			}
			addition := fx.send(t, fx.contributor, path, url.Values{
				"intent": {"add_behavior"}, "revision": {strconv.FormatUint(draft.Revision, 10)},
			}, nil)
			if addition.status != http.StatusConflict || !strings.Contains(addition.body, "Review latest draft") ||
				strings.Contains(addition.body, "Your submitted behavior") {
				t.Fatalf("stale addition did not offer explicit review without inventing content: HTTP %d", addition.status)
			}
			latest := loadBrowserDraft(t, fx.app, draft.ID)
			if latest.Revision != draft.Revision+1 || len(latest.Baseline.Behaviors) != 1 ||
				latest.Baseline.Behaviors[0].Text != "The newer saved behavior" {
				t.Fatal("either stale action altered the actual committed roster")
			}
		})
	}
}

func TestBrowserQueuedDraftSavePreservesEditsAfterConcurrentCommit(t *testing.T) {
	now := interruptionFixtureTime
	var pauseNextAudit atomic.Bool
	auditEntered, releaseAudit := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseAudit) })
	settings := board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time {
		if pauseNextAudit.CompareAndSwap(true, false) {
			close(auditEntered)
			<-releaseAudit
		}
		return now
	}}
	app, err := board.Open(filepath.Join(t.TempDir(), "queued-draft.db"), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	if err := app.Bootstrap("reviewer", browserPassword); err != nil {
		t.Fatal(err)
	}
	if err := app.SetAccount("reviewer", "contributor", browserPassword, []board.Role{board.Contributor}); err != nil {
		t.Fatal(err)
	}
	draft, err := app.CreateDraft("reviewer", "pilot", board.Baseline{
		ID: "QUEUED", ProductID: "DEMO", Behaviors: []board.Behavior{{
			ID: "QUEUED-BEHAVIOR", Revision: 1, Text: "Old editable behavior",
			Policy: board.Policy{ID: "QUEUED-POLICY", Revision: 1, MaxAgeHours: 24},
		}},
	}, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	visit := serveBrowserTestBoard(t, app)
	defer release()
	winner, winnerCSRF := loginSession(t, visit, "reviewer")
	loser, loserCSRF := loginSession(t, visit, "contributor")
	path := visit + "/p/pilot/drafts/" + draft.ID
	edit := func(csrf, text string) url.Values {
		form := draftForm(csrf, draft)
		form.Set("text", text)
		return form
	}
	type result struct {
		status int
		body   string
		err    error
	}
	read := func(reply *http.Response, err error) result {
		if err != nil {
			return result{err: err}
		}
		body, err := io.ReadAll(reply.Body)
		if closeErr := reply.Body.Close(); err == nil {
			err = closeErr
		}
		return result{status: reply.StatusCode, body: string(body), err: err}
	}
	winResult, loseResult := make(chan result, 1), make(chan result, 1)
	pauseNextAudit.Store(true)
	go func() {
		winResult <- read(winner.PostForm(path, edit(winnerCSRF, "Winner's committed version")))
	}()
	select {
	case <-auditEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first real write did not reach the transaction audit")
	}
	sent := make(chan error, 1)
	form := edit(loserCSRF, "Queued editor's copy")
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) { sent <- info.Err },
	}))
	go func() {
		loseResult <- read(loser.Do(request))
	}()
	select {
	case err := <-sent:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second editor did not send the actual form")
	}
	// The first write remains inside bbolt while the second editor reaches the
	// same save path. The response assertions hold for both conflict timings.
	select {
	case reply := <-loseResult:
		t.Fatalf("queued editor unexpectedly returned before the first commit: %d", reply.status)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	for _, outcome := range []struct {
		reply <-chan result
		code  int
		text  string
	}{{winResult, http.StatusSeeOther, ""}, {loseResult, http.StatusConflict, "Queued editor&#39;s copy"}} {
		select {
		case reply := <-outcome.reply:
			if reply.err != nil || reply.status != outcome.code || !strings.Contains(reply.body, outcome.text) {
				t.Errorf("concurrent save returned HTTP %d, want %d with %q: %v", reply.status, outcome.code, outcome.text, reply.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("HTTP %d outcome did not arrive", outcome.code)
		}
	}
	latest := loadBrowserDraft(t, app, draft.ID)
	if latest.Revision != draft.Revision+1 || latest.Baseline.Behaviors[0].Text != "Winner's committed version" {
		t.Fatal("losing concurrent edit changed the committed draft")
	}
}

func TestBrowserDraftFormKeepsExistingDecodingAndDuplicateRefusal(t *testing.T) {
	fx := newAuthorityFixture(t)
	_, _, approved, err := fx.app.Candidate("reviewer", "pilot", fx.candidateID)
	if err != nil {
		t.Fatal(err)
	}
	path := submitRedirect(t, fx.contributor.client, fx.visit+"/p/pilot/drafts", fx.contributor.csrf,
		url.Values{"baseline": {approved.Descriptor.Digest}})
	id := strings.TrimPrefix(path, "/p/pilot/drafts/")
	draft := loadBrowserDraft(t, fx.app, id)
	fields := draftForm(fx.contributor.csrf, draft)
	fields.Set("text", "Literal UTF-8 é, semicolon ;, ampersand &, plus +, and equals =")
	for _, invalid := range []struct {
		suffix string
		status int
	}{
		{"&text=%ZZ", http.StatusBadRequest},
		{"&%ZZ=key", http.StatusBadRequest},
		{"&text=unescaped;separator", http.StatusBadRequest},
		{"&text=second+value", http.StatusUnprocessableEntity},
	} {
		_, status := response(t, func(endpoint string) (*http.Response, error) {
			return fx.contributor.client.Post(endpoint, "application/x-www-form-urlencoded", strings.NewReader(fields.Encode()+invalid.suffix))
		}, fx.visit+path)
		if status != invalid.status || loadBrowserDraft(t, fx.app, id).Revision != draft.Revision {
			t.Fatalf("invalid %q returned HTTP %d with unexpected application state", invalid.suffix, status)
		}
	}
	_, status := response(t, func(endpoint string) (*http.Response, error) {
		return fx.contributor.client.Post(endpoint, "application/x-www-form-urlencoded; charset=UTF-8", strings.NewReader(fields.Encode()+"&&"))
	}, fx.visit+path)
	if status != http.StatusSeeOther {
		t.Fatalf("standard encoded form parameters with empty separators did not retain their semantics: HTTP %d", status)
	}
	if got := loadBrowserDraft(t, fx.app, id); got.Revision != draft.Revision+1 || got.Baseline.Behaviors[0].Text != fields.Get("text") {
		t.Fatal("URL escaping did not preserve the exact supported text")
	}
}

func TestBrowserTruncatedDraftBodyCannotSave(t *testing.T) {
	fx := newAuthorityFixture(t)
	_, _, approved, err := fx.app.Candidate("reviewer", "pilot", fx.candidateID)
	if err != nil {
		t.Fatal(err)
	}
	path := submitRedirect(t, fx.contributor.client, fx.visit+"/p/pilot/drafts", fx.contributor.csrf,
		url.Values{"baseline": {approved.Descriptor.Digest}})
	id := strings.TrimPrefix(path, "/p/pilot/drafts/")
	draft := loadBrowserDraft(t, fx.app, id)
	endpoint, err := url.Parse(fx.visit + path)
	if err != nil {
		t.Fatal(err)
	}
	cookies := fx.contributor.client.Jar.Cookies(endpoint)
	if len(cookies) != 1 || cookies[0].Name != "reb_session" {
		t.Fatal("authenticated test session cookie is unavailable")
	}
	conn, err := net.DialTimeout("tcp", endpoint.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	form := draftForm(fx.contributor.csrf, draft)
	form.Set("text", "This draft edit is sent with a missing suffix")
	encoded := form.Encode()
	if _, err := fmt.Fprintf(conn,
		"POST %s HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nCookie: %s\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: %d\r\n\r\n%s",
		path, endpoint.Host, fx.visit, cookies[0].String(), len(encoded), encoded[:len(encoded)-1]); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	if err := reply.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if reply.StatusCode != http.StatusBadRequest || loadBrowserDraft(t, fx.app, id).Revision != draft.Revision {
		t.Fatalf("truncated authorized draft form changed durable work: HTTP %d", reply.StatusCode)
	}
	submitRedirect(t, fx.contributor.client, fx.visit+path, fx.contributor.csrf, form)
	if got := loadBrowserDraft(t, fx.app, id); got.Revision != draft.Revision+1 || got.Baseline.Behaviors[0].Text != form.Get("text") {
		t.Fatal("complete retry did not retain the supported edit")
	}
}

func TestBrowserDraftWrongContentTypeNeverInterpretsAnEncodedEdit(t *testing.T) {
	fx := newAuthorityFixture(t)
	_, _, approved, err := fx.app.Candidate("reviewer", "pilot", fx.candidateID)
	if err != nil {
		t.Fatal(err)
	}
	path := submitRedirect(t, fx.contributor.client, fx.visit+"/p/pilot/drafts", fx.contributor.csrf,
		url.Values{"baseline": {approved.Descriptor.Digest}})
	id := strings.TrimPrefix(path, "/p/pilot/drafts/")
	draft := loadBrowserDraft(t, fx.app, id)
	fields := draftForm(fx.contributor.csrf, draft)
	fields.Set("text", "Only a supported form may save this text")
	for _, unsupported := range []struct {
		contentType string
		status      int
	}{
		{"text/plain", http.StatusForbidden},
		{"application/x-www-form-urlencoded-extra", http.StatusForbidden},
		{"application/x-www-form-urlencoded; charset", http.StatusBadRequest},
	} {
		page, status := response(t, func(endpoint string) (*http.Response, error) {
			return fx.contributor.client.Post(endpoint, unsupported.contentType, strings.NewReader(fields.Encode()))
		}, fx.visit+path)
		if status != unsupported.status || strings.Contains(page, "Only a supported form") ||
			loadBrowserDraft(t, fx.app, id).Revision != draft.Revision {
			t.Fatalf("wrong content type %q reached editing or reflected fields: HTTP %d", unsupported.contentType, status)
		}
	}
	submitRedirect(t, fx.contributor.client, fx.visit+path, fx.contributor.csrf, fields)
	if got := loadBrowserDraft(t, fx.app, id); got.Revision != draft.Revision+1 ||
		got.Baseline.Behaviors[0].Text != "Only a supported form may save this text" {
		t.Fatal("the same supported URL-encoded edit did not save")
	}
}
