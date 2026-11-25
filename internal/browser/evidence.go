package browser

import (
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"release-evidence-board/internal/board"
	"release-evidence-board/internal/upload"
)

type boardPage struct {
	board.Assessment
	Visible       []board.BehaviorAssessment
	State, Reason string
	PauseURL      string
}

type behaviorPage struct {
	board.Assessment
	Selected board.BehaviorAssessment
	Back     string
	PauseURL string
}

type intakePage struct {
	Candidate board.Candidate
	Baseline  board.ApprovedBaseline
	Preview   board.Preview
	Filename  string
}

type reportList struct {
	Candidate board.Candidate
	Baseline  board.ApprovedBaseline
	Reports   []board.Report
}

func (s *Server) candidatePath(id string) string { return s.projectPath() + "/candidates/" + id }

func (s *Server) candidate(w http.ResponseWriter, r *http.Request, view screen) {
	id := r.PathValue("id")
	assessment, err := s.board.Assess(view.Account.ID, view.Project, id)
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	query := r.URL.Query()
	data := boardPage{Assessment: assessment, State: query.Get("status"), Reason: query.Get("reason")}
	for _, behavior := range assessment.Behaviors {
		if data.State != "" && data.State != behavior.Status {
			continue
		}
		if data.Reason != "" && !hasReason(behavior.Reasons, data.Reason) {
			continue
		}
		data.Visible = append(data.Visible, behavior)
	}
	view.AutoRefresh = query.Get("refresh") != "off"
	pause := url.Values{"status": {data.State}, "reason": {data.Reason}}
	if view.AutoRefresh {
		pause.Set("refresh", "off")
	}
	data.PauseURL = s.candidatePath(id) + "?" + pause.Encode()
	view.Data = data
	notice := ""
	if query.Get("reused") == "1" {
		notice = "The execution and approved contract already belong to this retained candidate."
	}
	s.render(w, view, "board", "Candidate evidence", notice, http.StatusOK)
}

func hasReason(reasons []board.Reason, code string) bool {
	for _, reason := range reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

func (s *Server) getManifest(w http.ResponseWriter, r *http.Request, view screen) {
	_, manifest, _, err := s.board.Candidate(view.Account.ID, view.Project, r.PathValue("id"))
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	download(w, "manifest-"+manifest.Descriptor.Digest+".json", "application/json", []byte(manifest.Descriptor.Text))
}

func (s *Server) behavior(w http.ResponseWriter, r *http.Request, view screen) {
	id, behaviorID := r.PathValue("id"), r.PathValue("behavior")
	if behaviorID == "" {
		behaviorID = r.URL.Query().Get("id")
	}
	assessment, err := s.board.Assess(view.Account.ID, view.Project, id)
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	for _, behavior := range assessment.Behaviors {
		if behavior.ID != behaviorID {
			continue
		}
		query := r.URL.Query()
		previous := url.Values{"status": {query.Get("status")}, "reason": {query.Get("reason")}, "refresh": {query.Get("refresh")}}
		view.AutoRefresh = query.Get("refresh") != "off"
		pause := url.Values{"id": {behaviorID}, "status": {query.Get("status")}, "reason": {query.Get("reason")}}
		if view.AutoRefresh {
			pause.Set("refresh", "off")
		}
		view.Data = behaviorPage{Assessment: assessment, Selected: behavior,
			Back:     s.candidatePath(id) + "?" + previous.Encode() + "#behavior-" + behaviorID,
			PauseURL: s.candidatePath(id) + "/behaviors?" + pause.Encode()}
		s.render(w, view, "behavior", "Behavior "+behaviorID, "", http.StatusOK)
		return
	}
	s.serveError(w, view, board.ErrNotFound)
}

func (s *Server) intake(w http.ResponseWriter, r *http.Request, view screen) {
	if !view.Account.CanContribute() {
		s.serveError(w, view, board.ErrForbidden)
		return
	}
	candidate, _, baseline, err := s.board.Candidate(view.Account.ID, view.Project, r.PathValue("id"))
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	view.Data = intakePage{Candidate: candidate, Baseline: baseline}
	s.render(w, view, "intake", "Import results", "", http.StatusOK)
}

func (s *Server) importRetryDelay(account string, policy board.ImportRatePolicy) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := time.Now()
	windowStart := at.Add(-policy.Window)
	times := s.importRate[account]
	i := 0
	for i < len(times) && !times[i].After(windowStart) {
		i++
	}
	times = times[i:]
	if len(times) >= policy.Count {
		s.importRate[account] = times
		return times[len(times)-policy.Count].Add(policy.Window).Sub(at)
	}
	s.importRate[account] = append(times, at)
	return 0
}

func (s *Server) beginImport(w http.ResponseWriter, view screen) bool {
	if !view.Account.CanContribute() {
		s.recordRejected(view.Account.ID, true)
		s.serveError(w, view, board.ErrForbidden)
		return false
	}
	policy, err := s.board.ImportRate(view.Account.ID, view.Project)
	if err != nil {
		s.serveError(w, view, err)
		return false
	}
	if retry := s.importRetryDelay(view.Account.ID, policy); retry > 0 {
		s.recordRejected(view.Account.ID, true)
		// Round up a positive duration without overflowing at its maximum.
		seconds := int64((retry-1)/time.Second) + 1
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		s.render(w, view, "error", "Import request limit", "Try again when the import window has cleared.", http.StatusTooManyRequests)
		return false
	}
	return true
}

func (s *Server) preview(w http.ResponseWriter, r *http.Request, view screen) {
	if !s.beginImport(w, view) {
		return
	}
	id := r.PathValue("id")
	candidate, _, baseline, err := s.board.Candidate(view.Account.ID, view.Project, id)
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	view.Data = intakePage{Candidate: candidate, Baseline: baseline}
	if r.MultipartForm == nil || len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["results"]) != 1 || !formFields(r.PostForm) {
		s.render(w, view, "intake", "Import results", "Choose one results CSV file.", http.StatusUnprocessableEntity)
		return
	}
	f, info, err := r.FormFile("results")
	if err != nil {
		s.render(w, view, "intake", "Import results", "The CSV could not be read.", http.StatusUnprocessableEntity)
		return
	}
	raw, uploadError := upload.ReadAndClose(f, board.MaxCSVBytes+1)
	if uploadError != nil {
		s.render(w, view, "intake", "Import results", "The CSV could not be read.", http.StatusUnprocessableEntity)
		return
	}
	preview, err := s.board.PreviewImport(view.Account.ID, view.Project, id, raw)
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	view.Data = intakePage{Candidate: candidate, Baseline: baseline, Preview: preview, Filename: info.Filename}
	status := http.StatusOK
	notice := ""
	if len(preview.Issues) > 0 {
		status = http.StatusUnprocessableEntity
		notice = "Correct the producer CSV using the diagnostics below and select the corrected file."
	}
	s.render(w, view, "intake", "Review import", notice, status)
}

func (s *Server) commit(w http.ResponseWriter, r *http.Request, view screen) {
	if !s.beginImport(w, view) {
		return
	}
	if !formFields(r.PostForm, "preview") {
		s.serveError(w, view, board.ErrInvalid)
		return
	}
	receipt, err := s.board.CommitImport(view.Account.ID, view.Project, r.PostForm.Get("preview"), nil)
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	http.Redirect(w, r, s.projectPath()+"/receipts/"+receipt.ID, http.StatusSeeOther)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request, view screen) {
	if !formFields(r.PostForm, "preview") {
		s.serveError(w, view, board.ErrInvalid)
		return
	}
	if err := s.board.CancelPreview(view.Account.ID, view.Project, r.PostForm.Get("preview")); err != nil {
		s.serveError(w, view, err)
		return
	}
	http.Redirect(w, r, s.candidatePath(r.PathValue("id"))+"/import", http.StatusSeeOther)
}

func (s *Server) receipt(w http.ResponseWriter, r *http.Request, view screen) {
	receipt, err := s.board.Receipt(view.Account.ID, view.Project, r.PathValue("id"))
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	view.Data = receipt
	s.render(w, view, "receipt", "Import receipt", "", http.StatusOK)
}

func (s *Server) source(w http.ResponseWriter, r *http.Request, view screen) {
	id := r.PathValue("id")
	data, err := s.board.SourceCSV(view.Account.ID, view.Project, id)
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	download(w, "original-"+id+".csv", "text/csv; charset=utf-8", data)
}

func (s *Server) reports(w http.ResponseWriter, r *http.Request, view screen) {
	candidateID := r.PathValue("id")
	candidate, _, baseline, err := s.board.Candidate(view.Account.ID, view.Project, candidateID)
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	reports, err := s.board.Reports(view.Account.ID, view.Project, candidateID)
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	view.Data = reportList{Candidate: candidate, Baseline: baseline, Reports: reports}
	s.render(w, view, "reports", "Frozen reports", "", http.StatusOK)
}

func (s *Server) freeze(w http.ResponseWriter, r *http.Request, view screen) {
	if !formFields(r.PostForm) {
		s.serveError(w, view, board.ErrInvalid)
		return
	}
	report, err := s.board.Freeze(view.Account.ID, view.Project, r.PathValue("id"))
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	http.Redirect(w, r, s.projectPath()+"/reports/"+report.ID, http.StatusSeeOther)
}

func (s *Server) report(w http.ResponseWriter, r *http.Request, view screen) {
	record, data, err := s.board.ReportHTML(view.Account.ID, view.Project, r.PathValue("id"))
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("ETag", fmt.Sprintf("%q", record.SHA256))
	if record.IntegrityWarning {
		w.Header().Set("X-REB-Integrity-Warning", "assessment-suspended")
	}
	if r.URL.Query().Get("download") == "1" {
		download(w, record.ID+".html", "text/html; charset=utf-8", data)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	write(w, data)
}

func download(w http.ResponseWriter, name, contentType string, data []byte) {
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": strings.TrimSpace(name)}))
	w.Header().Set("Content-Type", contentType)
	write(w, data)
}
