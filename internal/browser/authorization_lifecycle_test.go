package browser

import (
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolterrors "go.etcd.io/bbolt/errors"

	"release-evidence-board/internal/board"
)

type observedAuthorizationAccount struct {
	account board.Account
	err     error
}

type pausedAuthorizationReader struct {
	*board.Board
	pause  atomic.Bool
	read   chan observedAuthorizationAccount
	resume <-chan struct{}
}

func (r *pausedAuthorizationReader) Account(id string) (board.Account, error) {
	account, err := r.Board.Account(id)
	if r.pause.Swap(false) {
		r.read <- observedAuthorizationAccount{account, err}
		<-r.resume
	}
	return account, err
}

func TestProtectedGetStopsWhenBoardClosesAfterAccountRead(t *testing.T) {
	const actor, project, password = "reviewer", "pilot", "synthetic-browser-password"
	app, err := board.Open(filepath.Join(t.TempDir(), "authorization.db"), board.Settings{ProjectID: project, ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := app.Bootstrap(actor, password); err != nil {
		t.Fatal(err)
	}
	resume := make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	reader := &pausedAuthorizationReader{Board: app, read: make(chan observedAuthorizationAccount, 1), resume: resume}
	expected, err := app.Account(actor)
	if err != nil || expected.ID != actor || !expected.CanView() {
		t.Fatalf("owned account was not available: %v", err)
	}
	if actual, err := reader.Account(actor); err != nil || !reflect.DeepEqual(actual, expected) {
		t.Fatalf("reader did not preserve the real account: %v", err)
	}
	if _, err := reader.Account("unassigned"); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("reader did not preserve the missing account: %v", err)
	}
	for _, held := range []bool{false, true} {
		if held {
			if err := app.SuspendAssessments(actor, project); err != nil {
				t.Fatal(err)
			}
		}
		if actual, err := reader.AssessmentSuspended(actor, project); err != nil || actual != held {
			t.Fatalf("reader did not preserve the real assessment hold %t: %t, %v", held, actual, err)
		}
	}
	if err := app.Revalidate(actor, project); err != nil {
		t.Fatal(err)
	}

	server := newServer(app, false, reader)
	t.Cleanup(server.Close)
	router := http.NewServeMux()
	redirect := server.loggedIn(func(w http.ResponseWriter, r *http.Request, _ screen) {
		http.Redirect(w, r, "/style.css", http.StatusSeeOther)
	})
	router.Handle("GET /p/{project}/authorization-check", http.NewCrossOriginProtection().Handler(server.sessions.LoadAndSave(redirect)))
	router.Handle("/", server)
	web := httptest.NewServer(router)
	t.Cleanup(web.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	type reply struct {
		status         int
		location, body string
		err            error
	}
	read := func(response *http.Response, err error) reply {
		if err != nil {
			return reply{err: err}
		}
		body, err := io.ReadAll(response.Body)
		err = errors.Join(err, response.Body.Close())
		return reply{response.StatusCode, response.Header.Get("Location"), string(body), err}
	}
	login := read(client.Get(web.URL + "/login"))
	_, tokenStart, present := strings.Cut(login.body, `name="_csrf" value="`)
	token, _, terminated := strings.Cut(tokenStart, `"`)
	if login.err != nil || login.status != http.StatusOK || !present || !terminated || token == "" {
		t.Fatalf("real login form unavailable: HTTP %d, %v", login.status, login.err)
	}
	signed := read(client.PostForm(web.URL+"/login", url.Values{"actor": {actor}, "password": {password}, "_csrf": {token}}))
	if signed.err != nil || signed.status != http.StatusSeeOther || signed.location != "/p/pilot" {
		t.Fatalf("real login failed: HTTP %d, %v", signed.status, signed.err)
	}
	path := web.URL + "/p/pilot/authorization-check"
	if healthy := read(client.Get(path)); healthy.err != nil || healthy.status != http.StatusSeeOther || healthy.location != "/style.css" {
		t.Fatalf("healthy guard did not admit the same redirect: HTTP %d, %v", healthy.status, healthy.err)
	}

	reader.pause.Store(true)
	response := make(chan reply, 1)
	go func() { response <- read(client.Get(path)) }()
	select {
	case observed := <-reader.read:
		if observed.err != nil || !reflect.DeepEqual(observed.account, expected) {
			t.Fatalf("protected request did not complete its real account read: %v", observed.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("protected request did not reach the account-read barrier")
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.AssessmentSuspended(actor, project); !errors.Is(err, bolterrors.ErrDatabaseNotOpen) {
		t.Fatalf("closed storage did not produce the real hold-read error: %v", err)
	}
	release()
	select {
	case blocked := <-response:
		if blocked.err != nil || blocked.status != http.StatusInternalServerError || blocked.location != "" ||
			!strings.Contains(blocked.body, "The operation could not finish.") || strings.Contains(blocked.body, "database not open") {
			t.Fatalf("protected GET did not stop on the second read: HTTP %d location %q, %v", blocked.status, blocked.location, blocked.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("protected request did not join after Board.Close")
	}
}
