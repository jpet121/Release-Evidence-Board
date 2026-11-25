package browser

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/alexedwards/scs/v2"
	"github.com/alexedwards/scs/v2/memstore"

	"release-evidence-board/internal/board"
)

//go:embed pages.html
var pageSource string

//go:embed style.css
var stylesheet []byte

var pages = template.Must(template.New("pages").Funcs(template.FuncMap{
	"projectURL":  projectPath,
	"utc":         board.FormatUTC,
	"optionalUTC": formatOptionalUTC,
	"icon":        statusIcon,
	"number":      strconv.Itoa,
}).Parse(pageSource))

func formatOptionalUTC(value time.Time) string {
	if value.IsZero() {
		return "none"
	}
	return board.FormatUTC(value)
}

type Server struct {
	board      *board.Board
	auth       authorizationReader
	handler    http.Handler
	sessions   *scs.SessionManager
	sessionDB  *memstore.MemStore
	closeOnce  sync.Once
	mu         sync.Mutex
	importRate map[string][]time.Time
}

type authorizationReader interface {
	Account(id string) (board.Account, error)
	AssessmentSuspended(actor, project string) (bool, error)
}

type screen struct {
	Project, Product, Title, CSRF, Notice string
	Account                               board.Account
	Data                                  any
	AutoRefresh                           bool
	IntegrityWarning                      bool
}

func New(app *board.Board, secure bool) *Server {
	return newServer(app, secure, app)
}

func newServer(app *board.Board, secure bool, auth authorizationReader) *Server {
	sessions := scs.New()
	// The pinned memory store accepts its expiry instant. The last valid
	// nanosecond is immediately before the 30-minute inactivity boundary.
	sessions.IdleTimeout = 30*time.Minute - time.Nanosecond
	sessions.Cookie.Name = "reb_session"
	sessions.Cookie.Secure = secure
	sessions.Cookie.SameSite = http.SameSiteStrictMode
	sessions.Cookie.HttpOnly = true
	sessions.Cookie.Persist = false
	sessions.HashTokenInStore = true
	sessions.ErrorFunc = sessionError
	store := sessions.Store.(*memstore.MemStore)
	server := &Server{board: app, auth: auth, sessions: sessions, sessionDB: store, importRate: make(map[string][]time.Time)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", server.home)
	mux.HandleFunc("GET /style.css", serveStylesheet)
	mux.HandleFunc("GET /login", server.login)
	mux.HandleFunc("POST /login", server.login)
	mux.HandleFunc("POST /logout", server.loggedIn(server.logout))
	mux.HandleFunc("GET /p/{project}", server.loggedIn(server.workspace))
	mux.HandleFunc("GET /p/{project}/baselines", server.loggedIn(server.baselines))
	mux.HandleFunc("GET /p/{project}/baselines/{digest}", server.loggedIn(server.getDescriptor))
	mux.HandleFunc("POST /p/{project}/drafts", server.loggedIn(server.newDraft))
	mux.HandleFunc("GET /p/{project}/drafts/{id}", server.loggedIn(server.draft))
	mux.HandleFunc("POST /p/{project}/drafts/{id}", server.loggedIn(server.editDraft))
	mux.HandleFunc("POST /p/{project}/drafts/{id}/approve", server.loggedIn(server.approve))
	mux.HandleFunc("GET /p/{project}/candidates/new", server.loggedIn(server.newCandidate))
	mux.HandleFunc("POST /p/{project}/candidates", server.loggedIn(server.createCandidate))
	mux.HandleFunc("GET /p/{project}/candidates/{id}", server.loggedIn(server.candidate))
	mux.HandleFunc("GET /p/{project}/candidates/{id}/manifest", server.loggedIn(server.getManifest))
	mux.HandleFunc("GET /p/{project}/candidates/{id}/behaviors", server.loggedIn(server.behavior))
	mux.HandleFunc("GET /p/{project}/candidates/{id}/behaviors/{behavior}", server.loggedIn(server.behavior))
	mux.HandleFunc("GET /p/{project}/candidates/{id}/import", server.loggedIn(server.intake))
	mux.HandleFunc("POST /p/{project}/candidates/{id}/preview", server.loggedIn(server.preview))
	mux.HandleFunc("POST /p/{project}/candidates/{id}/commit", server.loggedIn(server.commit))
	mux.HandleFunc("POST /p/{project}/candidates/{id}/cancel", server.loggedIn(server.cancel))
	mux.HandleFunc("GET /p/{project}/receipts/{id}", server.loggedIn(server.receipt))
	mux.HandleFunc("GET /p/{project}/receipts/{id}/source", server.loggedIn(server.source))
	mux.HandleFunc("GET /p/{project}/candidates/{id}/reports", server.loggedIn(server.reports))
	mux.HandleFunc("POST /p/{project}/candidates/{id}/reports", server.loggedIn(server.freeze))
	mux.HandleFunc("GET /p/{project}/reports/{id}", server.loggedIn(server.report))
	mux.HandleFunc("GET /p/{project}/health", server.loggedIn(server.health))
	mux.HandleFunc("POST /p/{project}/health/revalidate", server.loggedIn(server.revalidate))
	server.handler = http.NewCrossOriginProtection().Handler(sessions.LoadAndSave(mux))
	return server
}

func (s *Server) Close() { s.closeOnce.Do(s.sessionDB.StopCleanup) }

func sessionError(w http.ResponseWriter, _ *http.Request, _ error) {
	log.Print("session storage operation failed")
	http.Error(w, "Session unavailable. Sign in again.", http.StatusInternalServerError)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.handler.ServeHTTP(w, r)
}

func projectSegment(id string) string {
	if id == "." || id == ".." {
		// ~ is outside the configured identifier alphabet.
		return "~" + id
	}
	return id
}

func projectPath(id string) string { return "/p/" + projectSegment(id) }

func (s *Server) projectPath() string { return projectPath(s.board.ProjectID()) }

func serveStylesheet(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	write(w, stylesheet)
}

func write(w http.ResponseWriter, data []byte) {
	// A disconnected client cannot receive an alternate response. Record no
	// request URL, submitted data or response body in operational logs.
	if _, err := w.Write(data); err != nil {
		log.Print("response transmission interrupted")
	}
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if s.sessions.GetString(r.Context(), "actor") == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, s.projectPath(), http.StatusSeeOther)
}

type protectedHandler func(http.ResponseWriter, *http.Request, screen)

// URL encoding can expand each UTF-8 byte to three characters. Bound the
// behavior edit by the maximum approved roster and scalar lengths in C01.
const maxDraftFormBytes = board.MaxAttempts*(2000*utf8.UTFMax*3+256) + 4000*utf8.UTFMax*3 + (1 << 16)

func (s *Server) loggedIn(handler protectedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := s.sessions.GetString(r.Context(), "actor")
		if id == "" {
			if r.Method == http.MethodPost {
				s.recordRejected("", true)
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		account, err := s.auth.Account(id)
		if errors.Is(err, board.ErrNotFound) {
			if err := s.sessions.Destroy(r.Context()); err != nil {
				s.serveError(w, screen{}, err)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if err != nil {
			s.serveError(w, screen{}, err)
			return
		}
		if project := r.PathValue("project"); project != "" && project != projectSegment(s.board.ProjectID()) {
			if r.Method == http.MethodPost {
				s.recordRejected(id, true)
			}
			s.serveError(w, screen{}, board.ErrNotFound)
			return
		}
		view := screen{Account: account, Project: s.board.ProjectID(), Product: s.board.ProductID(),
			CSRF: s.sessions.GetString(r.Context(), "csrf")}
		if r.Method == http.MethodGet && account.CanView() {
			held, err := s.auth.AssessmentSuspended(id, view.Project)
			if err != nil {
				s.serveError(w, view, err)
				return
			}
			view.IntegrityWarning = held
			if held {
				w.Header().Set("X-REB-Integrity-Warning", "assessment-suspended")
			}
		}
		if r.Method == http.MethodPost {
			limit := int64(4 << 20)
			if strings.HasSuffix(r.URL.Path, "/preview") {
				limit = int64(board.MaxCSVBytes + (1 << 16))
			} else if account.CanContribute() && r.Pattern == "POST /p/{project}/drafts/{id}" &&
				strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
				limit = maxDraftFormBytes
			}
			if !s.form(w, r, view, limit) {
				return
			}
		}
		handler(w, r, view)
	}
}

func (s *Server) form(w http.ResponseWriter, r *http.Request, view screen, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	var err error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		err = r.ParseMultipartForm(limit)
	} else if limit == maxDraftFormBytes {
		err = parseDraftEditForm(r)
	} else {
		err = r.ParseForm()
	}
	if err != nil {
		s.recordRejected(view.Account.ID, false)
		s.render(w, view, "error", "Upload or form error", "Check the form and CSV size, then try again.", http.StatusBadRequest)
		return false
	}
	sent := r.PostForm.Get("_csrf")
	if view.CSRF == "" || subtle.ConstantTimeCompare([]byte(view.CSRF), []byte(sent)) != 1 {
		s.recordRejected(view.Account.ID, true)
		s.serveError(w, view, board.ErrForbidden)
		return false
	}
	return true
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	token := s.sessions.GetString(r.Context(), "csrf")
	if r.Method == http.MethodGet {
		if token == "" {
			token = s.setCSRF(r)
		}
		s.render(w, screen{CSRF: token}, "login", "Sign in", "", http.StatusOK)
		return
	}
	if !s.form(w, r, screen{CSRF: token}, 8<<10) {
		return
	}
	id := r.PostForm.Get("actor")
	password := r.PostForm.Get("password")
	account, err := s.board.Authenticate(id, password)
	if errors.Is(err, board.ErrForbidden) {
		s.recordRejected("", true)
		s.render(w, screen{CSRF: token}, "login", "Sign in", "The account or password did not match.", http.StatusUnauthorized)
		return
	}
	if err != nil {
		s.serveError(w, screen{}, err)
		return
	}
	if err := s.sessions.RenewToken(r.Context()); err != nil {
		s.serveError(w, screen{}, err)
		return
	}
	s.setCSRF(r)
	s.sessions.Put(r.Context(), "actor", account.ID)
	http.Redirect(w, r, s.projectPath(), http.StatusSeeOther)
}

func (s *Server) recordRejected(actor string, denied bool) {
	if err := s.board.AuditRejectedRequest(actor, denied); err != nil {
		log.Print("request rejection audit unavailable")
	}
}

func (s *Server) setCSRF(r *http.Request) string {
	var random [32]byte
	// crypto/rand.Read fills the buffer or terminates the process.
	rand.Read(random[:])
	token := hex.EncodeToString(random[:])
	s.sessions.Put(r.Context(), "csrf", token)
	return token
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, view screen) {
	if err := s.sessions.Destroy(r.Context()); err != nil {
		s.serveError(w, view, err)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) workspace(w http.ResponseWriter, r *http.Request, view screen) {
	if !view.Account.CanView() && view.Account.CanOperate() {
		http.Redirect(w, r, s.projectPath()+"/health", http.StatusSeeOther)
		return
	}
	candidates, err := s.board.Candidates(view.Account.ID, view.Project)
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	view.Data = candidates
	s.render(w, view, "workspace", "Release candidates", "", http.StatusOK)
}

func (s *Server) render(w http.ResponseWriter, view screen, which, title, notice string, status int) {
	view.Title, view.Notice = title, notice
	var response bytes.Buffer
	for _, name := range []string{"head", which, "foot"} {
		if err := pages.ExecuteTemplate(&response, name, view); err != nil {
			log.Print("screen rendering failed")
			http.Error(w, "The board could not render this screen.", http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	write(w, response.Bytes())
}

func (s *Server) serveError(w http.ResponseWriter, view screen, err error) {
	var code int
	var message string
	switch {
	case errors.Is(err, board.ErrForbidden):
		code = http.StatusForbidden
		message = "Your current role or session cannot perform this action. Return to your workspace and try again."
	case errors.Is(err, board.ErrNotFound):
		code = http.StatusNotFound
		message = "The requested record is unavailable."
	case errors.Is(err, board.ErrConflict):
		code = http.StatusConflict
		message = "This view is out of date. Refresh or preview again before retrying."
	case errors.Is(err, board.ErrInvalid):
		code = http.StatusUnprocessableEntity
		message = err.Error()
	case errors.Is(err, board.ErrIntegrity):
		code = http.StatusServiceUnavailable
		message = "Evidence integrity requires operator verification and reviewer revalidation. Open retained records or contact an operator."
	default:
		code = http.StatusInternalServerError
		message = "The operation could not finish. Try again or contact an operator."
		log.Print("board operation failed")
	}
	s.render(w, view, "error", http.StatusText(code), message, code)
}

func statusIcon(status string) string {
	switch status {
	case board.Pass, board.Sufficient:
		return "✓"
	case board.Fail, board.Blocked:
		return "×"
	case board.Stale:
		return "◷"
	default:
		return "?"
	}
}

func formFields(values url.Values, permitted ...string) bool {
	keys := map[string]bool{"_csrf": true}
	for _, key := range permitted {
		keys[key] = true
	}
	for key, entries := range values {
		if !keys[key] || len(entries) != 1 {
			return false
		}
	}
	return true
}
