package browser

import (
	"net/http"

	"release-evidence-board/internal/board"
)

func (s *Server) health(w http.ResponseWriter, r *http.Request, view screen) {
	result, err := s.board.Health(view.Account.ID, view.Project)
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	view.Data = result
	s.render(w, view, "health", "Operator health", "", http.StatusOK)
}

func (s *Server) revalidate(w http.ResponseWriter, r *http.Request, view screen) {
	if !formFields(r.PostForm) {
		s.serveError(w, view, board.ErrInvalid)
		return
	}
	if err := s.board.Revalidate(view.Account.ID, view.Project); err != nil {
		s.serveError(w, view, err)
		return
	}
	http.Redirect(w, r, s.projectPath(), http.StatusSeeOther)
}
