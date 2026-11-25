//go:build windows || linux

package browser

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

// This is a real temporary filesystem adapter for the SCS Store contract, not
// an error-returning stub. Production still uses New's original memory store.
type sessionFiles struct {
	dir    string
	mu     sync.Mutex
	events []sessionFileEvent
}

type sessionFileEvent struct {
	operation, path string
	found           bool
	err             error
}
type sessionFileRecord struct {
	Data   []byte
	Expiry time.Time
}

func (s *sessionFiles) path(token string) string {
	hash := sha256.Sum256([]byte(token))
	return filepath.Join(s.dir, hex.EncodeToString(hash[:])+".session")
}

func (s *sessionFiles) record(event sessionFileEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *sessionFiles) observations(reset bool) []sessionFileEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := append([]sessionFileEvent(nil), s.events...)
	if reset {
		s.events = nil
	}
	return result
}

func (s *sessionFiles) Find(token string) ([]byte, bool, error) {
	path := s.path(token)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		s.record(sessionFileEvent{operation: "find", path: path})
		return nil, false, nil
	}
	var stored sessionFileRecord
	if err == nil {
		err = json.Unmarshal(raw, &stored)
	}
	found := err == nil && time.Now().Before(stored.Expiry)
	s.record(sessionFileEvent{operation: "find", path: path, found: found, err: err})
	if !found {
		return nil, false, err
	}
	return stored.Data, true, nil
}

func (s *sessionFiles) Commit(token string, data []byte, expiry time.Time) error {
	path := s.path(token)
	raw, err := json.Marshal(sessionFileRecord{data, expiry})
	if err == nil {
		err = os.WriteFile(path, raw, 0600)
	}
	s.record(sessionFileEvent{operation: "commit", path: path, err: err})
	return err
}

func (s *sessionFiles) Delete(token string) error {
	path := s.path(token)
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	s.record(sessionFileEvent{operation: "delete", path: path, err: err})
	return err
}

func TestTicket80RealSessionFilesContract(t *testing.T) {
	store := &sessionFiles{dir: t.TempDir()}
	if _, found, err := store.Find("absent"); err != nil || found {
		t.Fatal("missing file was found")
	}
	for _, data := range []string{"first", "replacement"} {
		if err := store.Commit("token", []byte(data), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if got, found, err := store.Find("token"); err != nil || !found || string(got) != data {
			t.Fatal("real commit/find contract failed")
		}
	}
	if err := store.Commit("token", []byte("expired"), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Find("token"); err != nil || found {
		t.Fatal("expired session was found")
	}
	for range 2 {
		if err := store.Delete("token"); err != nil {
			t.Fatal(err)
		}
	}
	if _, found, err := store.Find("token"); err != nil || found {
		t.Fatal("deleted session was found")
	}
}

func TestTicket80SessionNativeDeleteRefusalCannotMasqueradeAsSuccess(t *testing.T) {
	if runSessionPermissionChild(t) {
		return
	}
	for _, action := range []string{"login", "logout", "removed-account"} {
		t.Run(action, func(t *testing.T) {
			app := newSessionBoard(t)
			if err := app.SetAccount("reviewer", "viewer", "synthetic-viewer-password", []board.Role{board.Viewer}); err != nil {
				t.Fatal(err)
			}
			server := New(app, false)
			t.Cleanup(server.Close)
			files := &sessionFiles{dir: t.TempDir()}
			// SCS resolves this exported composition interface per request.
			// Keep the original MemStore owned by Server.Close unchanged.
			server.sessions.Store = files
			web := httptest.NewServer(server)
			t.Cleanup(web.Close)
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Jar: jar, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			login := sessionRequest(t, client, web.URL+"/login", nil)
			if login.status != http.StatusOK {
				t.Fatal("login page unavailable")
			}
			csrf := sessionCSRF(t, login.body)
			credentials := url.Values{"actor": {"viewer"}, "password": {"synthetic-viewer-password"}, "_csrf": {csrf}}
			if action != "login" {
				if signed := sessionRequest(t, client, web.URL+"/login", credentials); signed.status != http.StatusSeeOther {
					t.Fatal("normal login failed")
				}
				workspace := sessionRequest(t, client, web.URL+"/p/pilot", nil)
				if workspace.status != http.StatusOK {
					t.Fatal("normal workspace unavailable")
				}
				csrf = sessionCSRF(t, workspace.body)
			}
			if action == "removed-account" {
				if err := app.RemoveAccount("reviewer", "viewer"); err != nil {
					t.Fatal(err)
				}
			}
			oldCookie := sessionCookie(t, jar, web.URL)
			if oldCookie == "" {
				t.Fatal("normal setup issued no session cookie")
			}
			var path string
			for _, event := range files.observations(false) {
				if event.operation == "commit" && event.err == nil {
					path = event.path
				}
			}
			if path == "" {
				t.Fatal("no actual committed session file")
			}
			release := blockNativeSessionDeletion(t, path)
			files.observations(true)
			address := web.URL + "/login"
			form := credentials
			if action == "logout" {
				address, form = web.URL+"/logout", url.Values{"_csrf": {csrf}}
			}
			if action == "removed-account" {
				address, form = web.URL+"/p/pilot", nil
			}
			failed := sessionRequest(t, client, address, form)
			if failed.status != http.StatusInternalServerError || failed.location != "" ||
				!strings.Contains(failed.body, "The operation could not finish.") || strings.Contains(failed.body, "Session unavailable") {
				t.Fatalf("native deletion refusal did not produce the handler's redacted HTML500: HTTP%d", failed.status)
			}
			events := files.observations(false)
			if len(events) != 3 || events[0].operation != "find" || !events[0].found || events[0].err != nil ||
				events[1].operation != "delete" || events[2].operation != "commit" || events[2].err != nil {
				t.Fatal("session lookup/delete/refesh ordering did not isolate handler failure")
			}
			for _, event := range events {
				if event.path != path {
					t.Fatal("operation selected a different stored session")
				}
			}
			var native *os.PathError
			if !errors.As(events[1].err, &native) || native.Op != "remove" || native.Path != path || !isNativeSessionDeleteRefusal(native.Err) {
				t.Fatal("deletion did not preserve the expected native refusal")
			}
			if sessionCookie(t, jar, web.URL) != oldCookie {
				t.Fatal("failed operation rotated or removed the session cookie")
			}
			if action == "login" {
				if anonymous := sessionRequest(t, client, web.URL+"/p/pilot", nil); anonymous.status != http.StatusSeeOther || anonymous.location != "/login" {
					t.Fatal("failed login granted an authenticated session")
				}
			}
			if action == "removed-account" && strings.Contains(failed.body, "Active candidates") {
				t.Fatal("removed account received protected content")
			}
			release()
			recovered := sessionRequest(t, client, address, form)
			if recovered.status != http.StatusSeeOther {
				t.Fatal("same action did not recover after native fault release")
			}
			if action == "login" {
				newCookie := sessionCookie(t, jar, web.URL)
				if recovered.location != "/p/pilot" || newCookie == "" || newCookie == oldCookie {
					t.Fatal("successful retry did not rotate/grant")
				}
				if workspace := sessionRequest(t, client, web.URL+"/p/pilot", nil); workspace.status != http.StatusOK ||
					!strings.Contains(workspace.body, "Active candidates") {
					t.Fatal("recovered login cookie did not authenticate protected workspace")
				}
			} else if recovered.location != "/login" || sessionCookie(t, jar, web.URL) != "" {
				t.Fatal("successful retry did not invalidate session")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("successful retry left old stored session")
			}
			t.Logf("Find and idle-refresh Commit succeeded; native Delete refused with %v; same HTTP action recovered", native.Err)
		})
	}
}

func TestTicket80MalformedGobUsesRedactedSessionMiddlewareFailure(t *testing.T) {
	server := New(newSessionBoard(t), false)
	t.Cleanup(server.Close)
	web := httptest.NewServer(server)
	t.Cleanup(web.Close)
	const token = "synthetic-malformed-gob-cookie"
	hash := sha256.Sum256([]byte(token))
	if err := server.sessionDB.Commit(base64.RawURLEncoding.EncodeToString(hash[:]), []byte("not a valid Gob record"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, web.URL+"/p/pilot", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: "reb_session", Value: token})
	response, err := web.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(response.Body)
	if err := errors.Join(readErr, response.Body.Close()); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusInternalServerError || response.Header.Get("Location") != "" ||
		string(raw) != "Session unavailable. Sign in again.\n" {
		t.Fatal("malformed actual memory-store Gob was not refused/redacted")
	}
}
