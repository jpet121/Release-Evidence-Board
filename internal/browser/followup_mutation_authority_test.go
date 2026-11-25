package browser_test

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

type authoritySession struct {
	actor  string
	client *http.Client
	csrf   string
}

type authorityFixture struct {
	app                                                *board.Board
	visit, candidateID                                 string
	reviewer, viewer, anonymous, contributor, operator authoritySession
}

type authorityState struct {
	baselines  []board.ApprovedBaseline
	drafts     []board.Draft
	candidates []board.Candidate
	reports    []board.Report
	held       bool
	status     string
	ledger     uint64
}

// Valid inputs test the project, session and role boundaries before testing
// successful authoring in the same service. Each refused write is attributed.
func TestFollowupHTTPAuthoringMutationAuthority(t *testing.T) {
	fx := newAuthorityFixture(t)
	var editable board.Draft
	fx.step(t, "draft-create", "/p/pilot/drafts", url.Values{"baseline_id": {"AUTHORITY-DRAFT"}}, nil, http.StatusSeeOther, func(t *testing.T, result authorityResponse) {
		editable = fx.readDraft(t, result.location)
		if editable.Baseline.ID != "AUTHORITY-DRAFT" || len(editable.Baseline.Behaviors) != 0 {
			t.Fatal("authorized draft creation did not retain the submitted identity")
		}
	})
	editPath := "/p/pilot/drafts/" + editable.ID
	fx.step(t, "draft-edit", editPath, url.Values{"revision": {strconv.FormatUint(editable.Revision, 10)}, "intent": {"add_behavior"}}, nil, http.StatusSeeOther, func(t *testing.T, result authorityResponse) {
		edited := fx.readDraft(t, result.location)
		if edited.ID != editable.ID || edited.Revision != editable.Revision+1 || len(edited.Baseline.Behaviors) != 1 {
			t.Fatal("authorized edit did not add exactly one behavior to the intended draft")
		}
	})

	_, _, original, err := fx.app.Candidate("reviewer", "pilot", fx.candidateID)
	if err != nil {
		t.Fatal(err)
	}
	contents := original.Baseline
	contents.ID = "AUTHORITY-APPROVAL"
	pending, err := fx.app.CreateDraft("reviewer", "pilot", contents, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var approvedDigest string
	fx.step(t, "baseline-approve", "/p/pilot/drafts/"+pending.ID+"/approve", url.Values{"revision": {strconv.FormatUint(pending.Revision, 10)}}, nil, http.StatusSeeOther, func(t *testing.T, result authorityResponse) {
		approvedDigest = fx.readDraft(t, result.location).ApprovedDigest
		approved, err := fx.app.Baseline("reviewer", "pilot", approvedDigest)
		if err != nil || approved.Approver != "reviewer" || approved.DraftID != pending.ID || approved.Baseline.ID != contents.ID || len(fx.state(t).baselines) != 2 {
			t.Fatalf("authorized approval did not record exactly one approved descriptor: %v", err)
		}
	}, fx.contributor, fx.operator)

	newBuild := strings.Repeat("b", 64)
	fx.step(t, "candidate-create", "/p/pilot/candidates", url.Values{
		"release": {"R2"}, "version": {"2.0"}, "baseline": {approvedDigest}, "build": {newBuild},
		"environment": {"E1"}, "prompt": {"not_applicable"}, "model": {"not_applicable"}, "knowledge": {"not_applicable"},
	}, nil, http.StatusSeeOther, func(t *testing.T, result authorityResponse) {
		id, valid := strings.CutPrefix(result.location, "/p/pilot/candidates/")
		if !valid || id == fx.candidateID {
			t.Fatal("authorized create did not redirect to a new candidate")
		}
		created, manifest, _, err := fx.app.Candidate("reviewer", "pilot", id)
		if err != nil || created.BaselineDigest != approvedDigest || manifest.Manifest.BuildDigest != newBuild || len(fx.state(t).candidates) != 2 {
			t.Fatalf("authorized candidate creation did not retain its material identity: %v", err)
		}
	})
}

var authorityPreviewToken = regexp.MustCompile(`name="preview" value="([^"]+)"`)
var authorityPreviewRevision = regexp.MustCompile(`data revision ([0-9]+)`)

func TestFollowupHTTPEvidenceMutationAuthority(t *testing.T) {
	fx := newAuthorityFixture(t)
	candidate, _, _, err := fx.app.Candidate("reviewer", "pilot", fx.candidateID)
	if err != nil {
		t.Fatal(err)
	}
	path := "/p/pilot/candidates/" + candidate.ID
	stamp := board.FormatUTC(time.Now().UTC().Add(-3 * time.Minute))
	raw := followupBrowserCSV(t, followupBrowserResult(candidate, "R1", "BEH1", "P1", "T1", "AUTHORITY-PASS", "AUTHORITY-PASS-RESULT", stamp, "PASS"))
	var previewToken string
	var previewRevision uint64
	fileHash := fixtureHash(raw)
	fx.step(t, "csv-preview", path+"/preview", nil, raw, http.StatusOK, func(t *testing.T, result authorityResponse) {
		match := authorityPreviewToken.FindStringSubmatch(result.body)
		revision := authorityPreviewRevision.FindStringSubmatch(result.body)
		if len(match) != 2 || len(revision) != 2 || !strings.Contains(result.body, "SUFFICIENT") || !strings.Contains(result.body, fileHash) {
			t.Fatal("valid CSV did not show a confirmable passing projection")
		}
		var err error
		previewRevision, err = strconv.ParseUint(revision[1], 10, 64)
		if err != nil {
			t.Fatalf("preview revision: %v", err)
		}
		previewToken = match[1]
		fx.requireDecision(t, "BLOCKED", 0)
	})
	fx.step(t, "csv-confirm", path+"/commit", url.Values{"preview": {previewToken}}, nil, http.StatusSeeOther, func(t *testing.T, result authorityResponse) {
		receiptID, valid := strings.CutPrefix(result.location, "/p/pilot/receipts/")
		if !valid {
			t.Fatal("authorized import did not redirect to a receipt")
		}
		receipt, err := fx.app.Receipt("reviewer", "pilot", receiptID)
		if err != nil || receipt.Hash != fileHash || receipt.Uploader != "reviewer" || receipt.LedgerHighWater != 1 || !slices.Equal(receipt.AddedRuns, []string{"AUTHORITY-PASS"}) {
			t.Fatalf("authorized import did not commit one exact run: %v", err)
		}
		stored, err := fx.app.SourceCSV("reviewer", "pilot", receiptID)
		if err != nil || !bytes.Equal(stored, raw) {
			t.Fatalf("source bytes differ from the authorized preview: %v", err)
		}
		replay, err := fx.app.PreviewImport("reviewer", "pilot", candidate.ID, raw)
		if err != nil || len(replay.Issues) != 0 || replay.Token == "" || replay.Revision != previewRevision+1 || replay.Hash != fileHash || replay.ExistingReceiptID != receipt.ID {
			t.Fatalf("original preview and receipt were not joined by one data revision: %+v, %v", replay, err)
		}
		if err := fx.app.CancelPreview("reviewer", "pilot", replay.Token); err != nil {
			t.Fatalf("cancel revision check preview: %v", err)
		}
		assessment, err := fx.app.Assess("reviewer", "pilot", candidate.ID)
		if err != nil || assessment.Status != "SUFFICIENT" || assessment.LedgerHighWater != 1 || len(assessment.Behaviors) != 1 {
			t.Fatalf("confirmed CSV did not produce the one required sufficient decision: %+v, %v", assessment, err)
		}
		behavior := assessment.Behaviors[0]
		if !behavior.Required || behavior.Policy.MinPassBasisPoints != 10000 || behavior.Status != "PASS" || behavior.Passed != 1 || behavior.Planned != 1 || behavior.CurrentRunID != "AUTHORITY-PASS" {
			t.Fatalf("confirmed behavior does not have the expected threshold and PASS for every attempt: %+v", behavior)
		}
		recordThresholdHTTPInstance(t, "AC-004-22.P001", map[string]any{}, "Exactly previewed bytes committed")
		recordThresholdHTTPInstance(t, "AC-007-11.P001", map[string]any{"thresholdBasisPoints": 10000, "allPass": true}, "PASS")
	})

	failedCSV := followupBrowserCSV(t, followupBrowserResult(candidate, "R1", "BEH1", "P1", "T1", "AUTHORITY-FAIL", "AUTHORITY-FAIL-RESULT", stamp, "FAIL"))
	staged, err := fx.app.PreviewImport("reviewer", "pilot", candidate.ID, failedCSV)
	if err != nil || staged.Token == "" || len(staged.Projected) != 1 || staged.Projected[0].Status != "BLOCKED" {
		t.Fatalf("cancel control did not stage a definitive blocker: %v", err)
	}
	fx.step(t, "csv-cancel", path+"/cancel", url.Values{"preview": {staged.Token}}, nil, http.StatusSeeOther, func(t *testing.T, result authorityResponse) {
		if result.location != path+"/import" {
			t.Fatal("authorized cancellation did not return to intake")
		}
		cancelled := fx.send(t, fx.reviewer, path+"/commit", url.Values{"preview": {staged.Token}}, nil)
		if cancelled.status != http.StatusNotFound {
			t.Fatalf("cancelled preview confirmation returned HTTP %d, want 404", cancelled.status)
		}
		fx.requireDecision(t, "SUFFICIENT", 1)
	})
	fx.step(t, "freeze-report", path+"/reports", nil, nil, http.StatusSeeOther, func(t *testing.T, result authorityResponse) {
		id, valid := strings.CutPrefix(result.location, "/p/pilot/reports/")
		reports := fx.state(t).reports
		if !valid || len(reports) != 1 || reports[0].ID != id || reports[0].Status != "SUFFICIENT" || reports[0].LedgerHighWater != 1 {
			t.Fatal("authorized freeze did not retain precisely one sufficient report for the confirmed evidence")
		}
		html, status := response(t, fx.viewer.client.Get, fx.visit+result.location+"?download=1")
		if status != http.StatusOK || !strings.Contains(html, "Historical assessment · SUFFICIENT") || !strings.Contains(html, "AUTHORITY-PASS-RESULT") {
			t.Fatal("the frozen assessment did not expose the confirmed evidence")
		}
	})

	if err := fx.app.SuspendAssessments("reviewer", "pilot"); err != nil || !fx.state(t).held {
		t.Fatalf("could not put the fixture under a healthy incident hold: %v", err)
	}
	fx.step(t, "reviewer-revalidate", "/p/pilot/health/revalidate", nil, nil, http.StatusSeeOther, func(t *testing.T, result authorityResponse) {
		if result.location != "/p/pilot" || fx.state(t).held {
			t.Fatal("authorized reviewer did not clear the healthy incident hold")
		}
		fx.requireDecision(t, "SUFFICIENT", 1)
	}, fx.operator)
}

func newAuthorityFixture(t *testing.T) authorityFixture {
	t.Helper()
	app, visit, candidateID := startTestBrowser(t)
	for actor, role := range map[string]board.Role{"test-contributor": board.Contributor, "test-operator": board.Operator} {
		if err := app.SetAccount("reviewer", actor, browserPassword, []board.Role{role}); err != nil {
			t.Fatal(err)
		}
	}
	login := func(actor string) authoritySession {
		client, csrf := loginSession(t, visit, actor)
		return authoritySession{actor, client, csrf}
	}
	reviewer, viewer := login("reviewer"), login("viewer")
	if reviewer.csrf == viewer.csrf {
		t.Fatal("independent login sessions returned the same CSRF token")
	}
	return authorityFixture{app: app, visit: visit, candidateID: candidateID,
		reviewer: reviewer, viewer: viewer, contributor: login("test-contributor"), operator: operatorAuthoritySession(t, visit),
		anonymous: authoritySession{actor: "anonymous", client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}},
	}
}

func operatorAuthoritySession(t *testing.T, visit string) authoritySession {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	form, status := response(t, client.Get, visit+"/login")
	initial := boundToken.FindStringSubmatch(form)
	if status != http.StatusOK || len(initial) != 2 {
		t.Fatal("operator could not read the login form")
	}
	_, status = post(t, client, visit+"/login", url.Values{"actor": {"test-operator"}, "password": {browserPassword}, "_csrf": {initial[1]}})
	if status != http.StatusSeeOther {
		t.Fatalf("operator login returned HTTP %d", status)
	}
	health, status := response(t, client.Get, visit+"/p/pilot/health")
	active := boundToken.FindStringSubmatch(health)
	if status != http.StatusOK || len(active) != 2 || !strings.Contains(health, "test-operator") {
		t.Fatal("operator-only session could not open its authorized health view")
	}
	return authoritySession{"test-operator", client, active[1]}
}

type authorityResponse struct {
	status                     int
	body, location, attachment string
}

func (fx authorityFixture) send(t *testing.T, session authoritySession, path string, fields url.Values, raw []byte) authorityResponse {
	t.Helper()
	var reply *http.Response
	var err error
	if raw == nil {
		form := url.Values{"_csrf": {session.csrf}}
		maps.Copy(form, fields)
		reply, err = session.client.PostForm(fx.visit+path, form)
	} else {
		var payload bytes.Buffer
		form := multipart.NewWriter(&payload)
		if err := form.WriteField("_csrf", session.csrf); err != nil {
			t.Fatal(err)
		}
		file, err := form.CreateFormFile("results", "authority-results.csv")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		reply, err = session.client.Post(fx.visit+path, form.FormDataContentType(), &payload)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(reply.Body)
	if closeErr := reply.Body.Close(); readErr != nil || closeErr != nil {
		t.Fatalf("reading HTTP response: %v, %v", readErr, closeErr)
	}
	return authorityResponse{reply.StatusCode, string(data), reply.Header.Get("Location"), reply.Header.Get("Content-Disposition")}
}

func (fx authorityFixture) step(t *testing.T, name, path string, fields url.Values, raw []byte, success int, verify func(*testing.T, authorityResponse), additionalDenied ...authoritySession) {
	t.Helper()
	if !t.Run(name, func(t *testing.T) {
		original := fx.state(t)
		wrongSession := fx.reviewer
		if fx.reviewer.csrf == "" || fx.viewer.csrf == "" || fx.reviewer.csrf == fx.viewer.csrf {
			t.Fatal("authority control requires two different live session tokens")
		}
		wrongSession.csrf = fx.viewer.csrf
		foreignPath := strings.Replace(path, "/p/pilot/", "/p/foreign/", 1)
		fx.deny(t, "foreign-project", fx.reviewer, foreignPath, fields, raw, http.StatusNotFound)
		fx.deny(t, "anonymous", fx.anonymous, path, fields, raw, http.StatusSeeOther)
		fx.deny(t, "wrong-session-token", wrongSession, path, fields, raw, http.StatusForbidden)
		fx.deny(t, "viewer", fx.viewer, path, fields, raw, http.StatusForbidden)
		for _, role := range additionalDenied {
			fx.deny(t, role.actor, role, path, fields, raw, http.StatusForbidden)
		}
		if !reflect.DeepEqual(fx.state(t), original) {
			t.Fatal("refused requests altered observable state before the authorized control")
		}
		accepted := fx.send(t, fx.reviewer, path, fields, raw)
		if accepted.status != success {
			t.Fatalf("authorized control returned HTTP %d, want %d", accepted.status, success)
		}
		verify(t, accepted)
		recordAuthorityRouteObservations(t, name)
		t.Logf("authorized HTTP=%d state verified after denied requests", success)
	}) {
		t.FailNow()
	}
}

// Each caller asserts an authorized control for this exact operation before
// recording the two direct route boundaries. The wider route inventory is
// verified separately.
func recordAuthorityRouteObservations(t *testing.T, name string) {
	t.Helper()
	routes := map[string]struct {
		operation       string
		viewer, foreign int
	}{
		"draft-create":        {"draft create", 1, 15},
		"draft-edit":          {"draft edit", 2, 16},
		"baseline-approve":    {"baseline approval", 3, 17},
		"candidate-create":    {"candidate creation", 4, 18},
		"csv-preview":         {"CSV preview", 5, 19},
		"csv-confirm":         {"CSV confirm", 6, 20},
		"csv-cancel":          {"cancel import review", 7, 21},
		"freeze-report":       {"freeze report", 8, 22},
		"reviewer-revalidate": {"restore revalidation", 12, 26},
	}
	route, exists := routes[name]
	if !exists {
		t.Fatalf("unknown authority route %s", name)
	}
	parameter := map[string]any{"requiredOperation": route.operation}
	recordThresholdHTTPInstance(t, fmt.Sprintf("AC-001-02.P%03d", route.viewer), parameter, "Every mutation is denied and data is unchanged")
	recordThresholdHTTPInstance(t, fmt.Sprintf("AC-001-03.P%03d", route.foreign), parameter, "No foreign data or mutation is exposed")
	switch name {
	case "draft-create":
		recordThresholdHTTPInstance(t, "AC-012-03.P001", map[string]any{"failedMutation": "authorization denial"}, "No success event; denial/failure outcome distinguishable")
		recordThresholdHTTPInstance(t, "AC-013-04.P001", map[string]any{"request": "unauthenticated browser mutation"}, "Authentication/CSRF controls reject")
		recordThresholdHTTPInstance(t, "AC-013-04.P002", map[string]any{"request": "authenticated forged browser mutation"}, "Authentication/CSRF controls reject")
	case "baseline-approve":
		recordThresholdHTTPInstance(t, "AC-001-01.P001", map[string]any{}, "Approval succeeds and actor is recorded")
		recordThresholdHTTPInstance(t, "AC-001-06.P001", map[string]any{}, "Denied")
		// This parameter covers the actual Contributor approval denial. The
		// separate evidence-override parameter has its own test.
		recordThresholdHTTPInstance(t, "AC-001-04.P001", map[string]any{"action": "baseline approval"}, "Both actions denied")
	}
}

func (fx authorityFixture) deny(t *testing.T, name string, session authoritySession, path string, fields url.Values, raw []byte, want int) {
	t.Helper()
	before := fx.audit(t)
	result := fx.send(t, session, path, fields, raw)
	if result.status != want {
		t.Fatalf("%s returned HTTP %d, want %d", name, result.status, want)
	}
	switch want {
	case http.StatusSeeOther:
		if result.location != "/login" {
			t.Fatal("anonymous mutation did not require reauthentication")
		}
	case http.StatusNotFound:
		if !strings.Contains(result.body, "The requested record is unavailable.") {
			t.Fatal("foreign response did not use the generic unavailable state")
		}
	case http.StatusForbidden:
		if !strings.Contains(result.body, "Your current role or session cannot perform this action.") {
			t.Fatalf("%s did not produce a role or session denial", name)
		}
	}
	if result.attachment != "" || strings.Contains(result.body, fx.candidateID) || strings.Contains(result.body, "AUTHORITY-PASS-RESULT") || strings.Contains(result.body, "AUTHORITY-DRAFT") {
		t.Fatalf("%s disclosed protected fixture content", name)
	}
	after := fx.audit(t)
	if len(after) <= len(before) || !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatalf("%s did not append a denial while preserving prior audit history", name)
	}
	for _, event := range after[len(before):] {
		if event.Actor != session.actor || event.Outcome != "denied" {
			t.Fatalf("%s audit actor=%q outcome=%q, want %s denied", name, event.Actor, event.Outcome, session.actor)
		}
	}
	t.Logf("%s HTTP=%d audit=denied actor=%s", name, want, session.actor)
}

func (fx authorityFixture) state(t *testing.T) authorityState {
	t.Helper()
	var current authorityState
	var err error
	current.baselines, current.drafts, err = fx.app.Baselines("reviewer", "pilot")
	if err != nil {
		t.Fatal(err)
	}
	current.candidates, err = fx.app.Candidates("reviewer", "pilot")
	if err != nil {
		t.Fatal(err)
	}
	current.reports, err = fx.app.Reports("reviewer", "pilot", fx.candidateID)
	if err != nil {
		t.Fatal(err)
	}
	current.held, err = fx.app.AssessmentSuspended("reviewer", "pilot")
	if err != nil {
		t.Fatal(err)
	}
	if !current.held {
		assessment, err := fx.app.Assess("reviewer", "pilot", fx.candidateID)
		if err != nil {
			t.Fatal(err)
		}
		current.status, current.ledger = assessment.Status, assessment.LedgerHighWater
	}
	return current
}

func (fx authorityFixture) requireDecision(t *testing.T, status string, ledger uint64) {
	t.Helper()
	assessment, err := fx.app.Assess("reviewer", "pilot", fx.candidateID)
	if err != nil || assessment.Status != status || assessment.LedgerHighWater != ledger {
		t.Fatalf("live decision=%s ledger=%d error=%v, want %s ledger=%d", assessment.Status, assessment.LedgerHighWater, err, status, ledger)
	}
}

func (fx authorityFixture) readDraft(t *testing.T, path string) board.Draft {
	t.Helper()
	id, valid := strings.CutPrefix(path, "/p/pilot/drafts/")
	if !valid {
		t.Fatal("authorized draft action did not redirect to a draft")
	}
	draft, err := fx.app.Draft("reviewer", "pilot", id)
	if err != nil {
		t.Fatal(err)
	}
	return draft
}

func (fx authorityFixture) audit(t *testing.T) []board.AuditEvent {
	t.Helper()
	events, err := fx.app.Audit("reviewer", "pilot")
	if err != nil {
		t.Fatal(err)
	}
	return events
}
