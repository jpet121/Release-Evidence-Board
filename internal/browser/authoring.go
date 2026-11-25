package browser

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"release-evidence-board/internal/board"
)

type catalog struct {
	Approved []board.ApprovedBaseline
	Drafts   []board.Draft
}

type draftPage struct {
	board.Draft
	LowerMarks []board.ThresholdChange
	retry      *draftRetry
}

type draftRetry struct {
	behavior int
	input    url.Values
}

type draftConflictPage struct {
	DraftID  string
	Revision uint64
	Input    url.Values
	Tests    []draftConflictTest
}

type draftConflictTest struct {
	ID, Attempts, Expected string
	MustPass               bool
}

func (page draftPage) InputValue(index int, key string, saved any) any {
	if page.retry != nil && page.retry.behavior == index {
		if values := page.retry.input[key]; len(values) == 1 {
			return values[0]
		}
	}
	return saved
}

func (page draftPage) InputChecked(index int, key string, saved bool) bool {
	if page.retry != nil && page.retry.behavior == index {
		values := page.retry.input[key]
		return len(values) == 1 && values[0] == "1"
	}
	return saved
}

type candidateForm struct {
	Input      board.CandidateInput
	Baselines  []board.ApprovedBaseline
	Candidates []board.Candidate
}

// A draft behavior form has at most 11 fixed controls and four per planned
// test. The draft endpoint already limits the body bytes before decoding.
const maxDraftFormFields = 11 + 4*board.MaxAttempts

func parseDraftEditForm(r *http.Request) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		return r.ParseForm()
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	query := string(body)
	if strings.Count(query, "&") >= maxDraftFormFields {
		return errors.New("number of draft form fields exceeded limit")
	}
	form := make(url.Values)
	for query != "" {
		var field string
		field, query, _ = strings.Cut(query, "&")
		if field == "" {
			continue
		}
		if strings.Contains(field, ";") {
			return errors.New("invalid semicolon separator in draft form")
		}
		key, value, _ := strings.Cut(field, "=")
		key, err = url.QueryUnescape(key)
		if err != nil {
			return err
		}
		value, err = url.QueryUnescape(value)
		if err != nil {
			return err
		}
		form.Add(key, value)
	}
	r.PostForm = form
	return r.ParseForm()
}

func (s *Server) baselines(w http.ResponseWriter, r *http.Request, view screen) {
	approved, drafts, err := s.board.Baselines(view.Account.ID, view.Project)
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	view.Data = catalog{Approved: approved, Drafts: drafts}
	s.render(w, view, "baselines", "Baselines", "", http.StatusOK)
}

func (s *Server) newDraft(w http.ResponseWriter, r *http.Request, view screen) {
	if !view.Account.CanContribute() {
		s.recordRejected(view.Account.ID, true)
		s.serveError(w, view, board.ErrForbidden)
		return
	}
	if !formFields(r.PostForm, "baseline_id", "baseline", "draft") {
		s.serveError(w, view, board.ErrInvalid)
		return
	}
	options := board.DraftOptions{FromBaseline: r.PostForm.Get("baseline"), FromDraft: r.PostForm.Get("draft")}
	baseline := board.Baseline{ID: r.PostForm.Get("baseline_id"), ProductID: view.Product}
	switch {
	case options.FromBaseline != "":
		approved, err := s.board.Baseline(view.Account.ID, view.Project, options.FromBaseline)
		if err != nil {
			s.serveError(w, view, err)
			return
		}
		baseline = approved.Baseline
	case options.FromDraft != "":
		parent, err := s.board.Draft(view.Account.ID, view.Project, options.FromDraft)
		if err != nil {
			s.serveError(w, view, err)
			return
		}
		baseline = parent.Baseline
		if id := r.PostForm.Get("baseline_id"); id != "" {
			baseline.ID, baseline.Revision = id, 1
		}
	}
	draft, err := s.board.CreateDraft(view.Account.ID, view.Project, baseline, options)
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	http.Redirect(w, r, s.projectPath()+"/drafts/"+draft.ID, http.StatusSeeOther)
}

func (s *Server) draft(w http.ResponseWriter, r *http.Request, view screen) {
	draft, err := s.board.Draft(view.Account.ID, view.Project, r.PathValue("id"))
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	s.showDraft(w, view, draft, "", http.StatusOK, nil)
}

func (s *Server) showDraft(w http.ResponseWriter, view screen, draft board.Draft, notice string, status int, retry *draftRetry) {
	page := draftPage{Draft: draft, retry: retry}
	if view.Account.CanContribute() && draft.ApprovedDigest == "" {
		changes, err := s.board.ReviewThresholds(view.Account.ID, view.Project, draft.ID)
		if err != nil {
			s.serveError(w, view, err)
			return
		}
		page.LowerMarks = changes
	}
	view.Data = page
	s.render(w, view, "draft", "Draft "+draft.Baseline.ID, notice, status)
}

func (s *Server) showDraftConflict(w http.ResponseWriter, view screen, id string, revision uint64, input url.Values) {
	page := draftConflictPage{DraftID: id, Revision: revision, Input: input}
	for i := 0; i < board.MaxAttempts; i++ {
		suffix := strconv.Itoa(i)
		if !input.Has("test_" + suffix) {
			break
		}
		page.Tests = append(page.Tests, draftConflictTest{
			ID: input.Get("test_" + suffix), Attempts: input.Get("attempts_" + suffix),
			Expected: input.Get("expected_" + suffix), MustPass: input.Get("must_pass_"+suffix) == "1",
		})
	}
	view.Data = page
	s.render(w, view, "draft_conflict", "Draft changed", "Your edits were not saved.", http.StatusConflict)
}

func (s *Server) editDraft(w http.ResponseWriter, r *http.Request, view screen) {
	if !view.Account.CanContribute() {
		s.recordRejected(view.Account.ID, true)
		s.serveError(w, view, board.ErrForbidden)
		return
	}
	draft, err := s.board.Draft(view.Account.ID, view.Project, r.PathValue("id"))
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	revision, err := strconv.ParseUint(r.PostForm.Get("revision"), 10, 64)
	if err != nil {
		s.recordRejected(view.Account.ID, false)
		s.serveError(w, view, board.ErrConflict)
		return
	}
	if revision != draft.Revision || draft.ApprovedDigest != "" {
		s.recordRejected(view.Account.ID, false)
		s.showDraftConflict(w, view, draft.ID, revision, r.PostForm)
		return
	}
	contents := draft.Baseline
	contents.Behaviors = slices.Clone(contents.Behaviors)
	intent := r.PostForm.Get("intent")
	index, parseIndex := strconv.Atoi(r.PostForm.Get("behavior_index"))
	switch {
	case intent == "add_behavior":
		if !formFields(r.PostForm, "revision", "intent") {
			s.recordRejected(view.Account.ID, false)
			s.serveError(w, view, board.ErrInvalid)
			return
		}
		suffix := strconv.Itoa(len(contents.Behaviors) + 1)
		contents.Behaviors = append(contents.Behaviors, board.Behavior{
			ID: "B-" + suffix, Required: true, Revision: 1,
			Policy: board.Policy{ID: "P-" + suffix, Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
			Tests:  []board.TestPlan{{ID: "T-" + suffix, MustPass: true, PlannedAttempts: 1}},
		})
	case parseIndex != nil || index < 0 || index >= len(contents.Behaviors):
		s.recordRejected(view.Account.ID, false)
		s.serveError(w, view, board.ErrInvalid)
		return
	case intent == "remove_behavior":
		if !validBehaviorForm(r, contents.Behaviors[index]) {
			s.recordRejected(view.Account.ID, false)
			s.serveError(w, view, board.ErrInvalid)
			return
		}
		contents.Behaviors = slices.Delete(contents.Behaviors, index, index+1)
	default:
		behavior, err := parseBehavior(r, contents.Behaviors[index], intent)
		if err != nil {
			s.recordRejected(view.Account.ID, false)
			s.showDraft(w, view, draft, err.Error(), http.StatusUnprocessableEntity,
				&draftRetry{behavior: index, input: r.PostForm})
			return
		}
		contents.Behaviors[index] = behavior
	}
	_, err = s.board.SaveDraft(view.Account.ID, view.Project, draft.ID, revision, contents)
	if errors.Is(err, board.ErrConflict) {
		s.showDraftConflict(w, view, draft.ID, revision, r.PostForm)
		return
	}
	if err != nil {
		draft.Baseline = contents
		s.showDraft(w, view, draft, err.Error(), http.StatusUnprocessableEntity, nil)
		return
	}
	http.Redirect(w, r, s.projectPath()+"/drafts/"+draft.ID, http.StatusSeeOther)
}

func parseBehavior(r *http.Request, base board.Behavior, intent string) (board.Behavior, error) {
	if !validBehaviorForm(r, base) {
		return base, fmt.Errorf("%w: behavior form contains unknown, repeated or malformed values", board.ErrInvalid)
	}
	values := r.PostForm
	for _, key := range []string{"behavior_id", "text", "policy_id", "minimum", "max_age"} {
		if !values.Has(key) {
			return base, fmt.Errorf("%w: missing behavior form field", board.ErrInvalid)
		}
	}
	mark, markError := strconv.Atoi(values.Get("minimum"))
	age, ageError := strconv.Atoi(values.Get("max_age"))
	if markError != nil || ageError != nil {
		return base, fmt.Errorf("enter an integer pass mark and maximum age")
	}
	base.ID, base.Text, base.Required = values.Get("behavior_id"), values.Get("text"), values.Get("required") == "1"
	base.Policy.ID, base.Policy.MinPassBasisPoints, base.Policy.MaxAgeHours = values.Get("policy_id"), mark, age
	tests := make([]board.TestPlan, 0, len(base.Tests)+1)
	for i := range base.Tests {
		suffix := strconv.Itoa(i)
		if !values.Has("test_"+suffix) || !values.Has("expected_"+suffix) {
			return base, fmt.Errorf("%w: missing test form field", board.ErrInvalid)
		}
		attempts, err := strconv.Atoi(values.Get("attempts_" + suffix))
		if err != nil {
			return base, fmt.Errorf("enter an integer planned-attempt count for test %d", i+1)
		}
		tests = append(tests, board.TestPlan{ID: values.Get("test_" + suffix), ExpectedOutcome: values.Get("expected_" + suffix),
			MustPass: values.Get("must_pass_"+suffix) == "1", PlannedAttempts: attempts})
	}
	switch intent {
	case "add_test":
		tests = append(tests, board.TestPlan{ID: base.ID + "-T" + strconv.Itoa(len(tests)+1), PlannedAttempts: 1})
	case "remove_test":
		target, err := strconv.Atoi(values.Get("test_remove"))
		if err != nil || target < 0 || target >= len(tests) {
			return base, board.ErrInvalid
		}
		tests = slices.Delete(tests, target, target+1)
	case "save_behavior":
	default:
		return base, board.ErrInvalid
	}
	base.Tests = tests
	return base, nil
}

func validBehaviorForm(r *http.Request, baseline board.Behavior) bool {
	keys := []string{"revision", "intent", "behavior_index", "behavior_id", "text", "required", "policy_id",
		"minimum", "max_age", "test_remove"}
	checkboxes := []string{"required"}
	for i := range baseline.Tests {
		suffix := strconv.Itoa(i)
		keys = append(keys, "test_"+suffix, "expected_"+suffix, "attempts_"+suffix, "must_pass_"+suffix)
		checkboxes = append(checkboxes, "must_pass_"+suffix)
	}
	if !formFields(r.PostForm, keys...) {
		return false
	}
	for _, key := range checkboxes {
		if r.PostForm.Has(key) && r.PostForm.Get(key) != "1" {
			return false
		}
	}
	return true
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request, view screen) {
	revision, err := strconv.ParseUint(r.PostForm.Get("revision"), 10, 64)
	if err != nil || !formFields(r.PostForm, "revision") {
		s.serveError(w, view, board.ErrInvalid)
		return
	}
	id := r.PathValue("id")
	_, err = s.board.Approve(view.Account.ID, view.Project, id, revision)
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	http.Redirect(w, r, s.projectPath()+"/drafts/"+id, http.StatusSeeOther)
}

func (s *Server) getDescriptor(w http.ResponseWriter, r *http.Request, view screen) {
	approved, err := s.board.Baseline(view.Account.ID, view.Project, r.PathValue("digest"))
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	download(w, "baseline-"+approved.Descriptor.Digest+".json", "application/json", []byte(approved.Descriptor.Text))
}

func (s *Server) newCandidate(w http.ResponseWriter, r *http.Request, view screen) {
	form := candidateForm{Input: board.CandidateInput{
		BaselineDigest: r.URL.Query().Get("baseline"), PredecessorID: r.URL.Query().Get("predecessor"),
		Manifest: board.Manifest{PromptDigest: "not_applicable", ModelConfigDigest: "not_applicable", KnowledgeDigest: "not_applicable"},
	}}
	if form.Input.PredecessorID != "" {
		prior, manifest, _, err := s.board.Candidate(view.Account.ID, view.Project, form.Input.PredecessorID)
		if err != nil {
			s.serveError(w, view, err)
			return
		}
		form.Input.Manifest = manifest.Manifest
		form.Input.DisplayVersion = prior.DisplayVersion
		if form.Input.BaselineDigest == "" {
			form.Input.BaselineDigest = prior.BaselineDigest
		}
	}
	s.showCandidateForm(w, view, form, "", http.StatusOK)
}

func (s *Server) showCandidateForm(w http.ResponseWriter, view screen, form candidateForm, notice string, status int) {
	if !view.Account.CanContribute() {
		s.serveError(w, view, board.ErrForbidden)
		return
	}
	approved, _, err := s.board.Baselines(view.Account.ID, view.Project)
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	form.Baselines = approved
	form.Candidates, err = s.board.Candidates(view.Account.ID, view.Project)
	if err != nil {
		s.serveError(w, view, err)
		return
	}
	view.Data = form
	s.render(w, view, "new_candidate", "Create candidate", notice, status)
}

func (s *Server) createCandidate(w http.ResponseWriter, r *http.Request, view screen) {
	values := r.PostForm
	if r.MultipartForm != nil && len(r.MultipartForm.File) != 0 ||
		!formFields(values, "release", "version", "display", "baseline", "build", "prompt", "model", "knowledge", "environment", "predecessor", "correction") {
		s.serveError(w, view, board.ErrInvalid)
		return
	}
	input := board.CandidateInput{
		Manifest: board.Manifest{ProductID: view.Product, ReleaseID: values.Get("release"), ApplicationVersion: values.Get("version"),
			BuildDigest: values.Get("build"), PromptDigest: values.Get("prompt"), ModelConfigDigest: values.Get("model"),
			KnowledgeDigest: values.Get("knowledge"), EnvironmentID: values.Get("environment")},
		BaselineDigest: values.Get("baseline"), DisplayVersion: values.Get("display"), PredecessorID: values.Get("predecessor"), Correction: values.Get("correction"),
	}
	candidate, reused, err := s.board.CreateCandidate(view.Account.ID, view.Project, input)
	if err != nil {
		s.showCandidateForm(w, view, candidateForm{Input: input}, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	target := s.candidatePath(candidate.ID)
	if reused {
		target += "?reused=1"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
