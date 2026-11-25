package browser_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"

	"release-evidence-board/internal/board"
	"release-evidence-board/internal/browser"
)

func FuzzHTTPLoginForm(f *testing.F) {
	for _, seed := range []struct {
		shape uint8
		body  string
	}{
		{0, "actor=*&password=x&_csrf=fixture-csrf"}, {0, "_csrf=%"}, {0, "password=missing-token"},
		{1, "--fixture-boundary\r\nContent-Disposition: form-data; name=\"_csrf\"\r\n\r\nfixture-csrf\r\n--fixture-boundary--\r\n"},
		{1, "invalid multipart boundary"}, {2, "_csrf=fixture-csrf"}, {0, strings.Repeat("a", 8193)},
	} {
		f.Add(seed.shape, seed.body)
	}
	path := filepath.Join(f.TempDir(), "no-accounts.db")
	store, err := board.Open(path, board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() {
		if err := errors.Join(store.Close(), inspectRejectedFormStore(path)); err != nil {
			f.Error(err)
		}
	})
	server := browser.New(store, false)
	f.Cleanup(server.Close)
	login := httptest.NewRecorder()
	server.ServeHTTP(login, httptest.NewRequest(http.MethodGet, "/login", nil))
	match := regexp.MustCompile(`name="_csrf" value="([a-zA-Z0-9]+)"`).FindSubmatch(login.Body.Bytes())
	if login.Code != http.StatusOK || len(match) != 2 {
		f.Fatalf("login page did not provide a CSRF token: HTTP %d", login.Code)
	}
	token := string(match[1])
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		f.Fatal("login page did not bind a session cookie")
	}
	// Exercise authentication rejection once per fixture. Generated cases fuzz
	// parsing and CSRF, where 10,000 guesses need no bcrypt work.
	valid := url.Values{"_csrf": {token}, "actor": {"reserved-absent-fixture-actor"}, "password": {"fixture"}}
	probe := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(valid.Encode()))
	probe.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range cookies {
		probe.AddCookie(cookie)
	}
	rejected := httptest.NewRecorder()
	server.ServeHTTP(rejected, probe)
	if rejected.Code != http.StatusUnauthorized || rejected.Header().Get("Location") != "" {
		f.Fatalf("valid form for absent account yielded HTTP %d", rejected.Code)
	}
	f.Fuzz(func(t *testing.T, shape uint8, initial string) {
		if len(initial) > 1<<15 {
			initial = initial[:1<<15]
		}
		body := strings.ReplaceAll(initial, "fixture-csrf", "invalid-fixture-csrf")
		request := httptest.NewRequest(http.MethodPost, "/login", bytes.NewBufferString(body))
		for _, cookie := range cookies {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		want := http.StatusForbidden
		switch shape % 3 {
		case 0:
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			params, parseErr := url.ParseQuery(body)
			if len(body) > 8192 || parseErr != nil {
				want = http.StatusBadRequest
			} else if params.Get("_csrf") == token {
				want = http.StatusUnauthorized
			}
		case 1:
			request.Header.Set("Content-Type", "multipart/form-data; boundary=fixture-boundary")
			want = 0
		case 2:
			request.Header.Set("Content-Type", "text/plain")
		}
		server.ServeHTTP(response, request)
		if want != 0 && response.Code != want {
			t.Fatalf("expected HTTP %d, got %d", want, response.Code)
		}
		if response.Code != http.StatusBadRequest && response.Code != http.StatusForbidden && response.Code != http.StatusUnauthorized {
			t.Fatalf("login without accounts yielded HTTP %d", response.Code)
		}
		if response.Header().Get("Location") != "" {
			t.Fatal("rejected unauthenticated form should not redirect to an authority-bearing view")
		}
	})
}

func inspectRejectedFormStore(path string) error {
	db, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true})
	if err != nil {
		return err
	}
	verification := db.View(func(transaction *bolt.Tx) error {
		metadata := transaction.Bucket([]byte("meta"))
		if metadata == nil || string(metadata.Get([]byte("project"))) != "pilot" ||
			string(metadata.Get([]byte("product"))) != "DEMO" || string(metadata.Get([]byte("schema"))) != "reb-store/1" ||
			metadata.Stats().KeyN != 3 {
			return errors.New("form changed project metadata")
		}
		audit := transaction.Bucket([]byte("audit"))
		auditDigests := transaction.Bucket([]byte("audit_metadata_sha256"))
		if audit == nil || auditDigests == nil {
			return errors.New("rejected form audit and exact companion digests must exist")
		}
		return transaction.ForEach(func(name []byte, bucket *bolt.Bucket) error {
			switch string(name) {
			case "meta":
				return nil
			case "audit":
				return bucket.ForEach(func(key []byte, value []byte) error {
					var event board.AuditEvent
					if err := json.Unmarshal(value, &event); err != nil {
						return err
					}
					if event.Actor != "anonymous" || event.Action != "request.reject" || len(event.References) != 0 ||
						(event.Outcome != "denied" && event.Outcome != "failure") {
						return errors.New("unexpected audit write from rejected form")
					}
					if event.Sequence == 0 || string(key) != fmt.Sprintf("%020d", event.Sequence) ||
						string(auditDigests.Get(key)) != fmt.Sprintf("%x", sha256.Sum256(value)) {
						return errors.New("rejection audit sequence or companion digest differs")
					}
					return nil
				})
			case "audit_metadata_sha256":
				return bucket.ForEach(func(key []byte, _ []byte) error {
					if audit.Get(key) == nil {
						return errors.New("rejection audit digest lacks an exact audit event")
					}
					return nil
				})
			default:
				if key, _ := bucket.Cursor().First(); key != nil {
					return fmt.Errorf("rejected form wrote to %s", name)
				}
				return nil
			}
		})
	})
	return errors.Join(verification, db.Close())
}
