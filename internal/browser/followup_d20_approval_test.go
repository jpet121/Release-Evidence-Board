package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
	"release-evidence-board/internal/browser"
)

const approvalPeer = "independent-reviewer"

type thresholdHTTPFixture struct {
	app      *board.Board
	visit    string
	old      board.Candidate
	approved board.ApprovedBaseline
	reducer  authoritySession
	peer     authoritySession
	adverse  []byte
}

func newThresholdHTTPFixture(t *testing.T) thresholdHTTPFixture {
	t.Helper()
	app, err := board.Open(filepath.Join(t.TempDir(), "approval.db"), board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
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
	if err := app.SetAccount("reviewer", approvalPeer, browserPassword, []board.Role{board.Reviewer}); err != nil {
		t.Fatal(err)
	}
	draft, err := app.CreateDraft("reviewer", "pilot", board.Baseline{
		ProductID: "DEMO", ID: "B1", Revision: 1,
		Behaviors: []board.Behavior{{ID: "BEH1", Revision: 1, Required: true, Text: "Two independent release checks",
			Policy: board.Policy{ID: "P1", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
			Tests: []board.TestPlan{
				{ID: "T1", ExpectedOutcome: "Mandatory check passes", MustPass: true, PlannedAttempts: 1},
				{ID: "T2", ExpectedOutcome: "Second check passes", PlannedAttempts: 1},
			},
		}},
	}, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := app.Approve("reviewer", "pilot", draft.ID, draft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	candidate, reused, err := app.CreateCandidate("reviewer", "pilot", board.CandidateInput{
		BaselineDigest: approved.Descriptor.Digest,
		Manifest: board.Manifest{ProductID: "DEMO", ReleaseID: "R1", ApplicationVersion: "1.0",
			BuildDigest: strings.Repeat("a", 64), PromptDigest: "not_applicable", ModelConfigDigest: "not_applicable",
			KnowledgeDigest: "not_applicable", EnvironmentID: "E1"},
	})
	if err != nil || reused {
		t.Fatalf("higher threshold candidate fixture failed: %v", err)
	}
	server := browser.New(app, false)
	t.Cleanup(server.Close)
	web := httptest.NewServer(server)
	t.Cleanup(web.Close)
	visit := web.URL
	reviewerClient, reviewerCSRF := loginSession(t, visit, "reviewer")
	peerClient, peerCSRF := loginSession(t, visit, approvalPeer)
	stamp := board.FormatUTC(time.Now().UTC().Add(-time.Minute))
	csv := followupBrowserCSV(t,
		followupBrowserResult(candidate, "R1", "BEH1", "P1", "T1", "REVIEW-FAIL", "PASS-RESULT", stamp, "PASS"),
		followupBrowserResult(candidate, "R1", "BEH1", "P1", "T2", "REVIEW-FAIL", "FAILED-RESULT", stamp, "FAIL"))
	return thresholdHTTPFixture{app: app, visit: visit, old: candidate, approved: approved,
		reducer: authoritySession{"reviewer", reviewerClient, reviewerCSRF},
		peer:    authoritySession{approvalPeer, peerClient, peerCSRF}, adverse: csv}
}

func (fx thresholdHTTPFixture) pendingDraft(t *testing.T, path, policy string) board.Draft {
	t.Helper()
	base := fx.approved.Baseline
	base.Behaviors = slices.Clone(base.Behaviors)
	base.Behaviors[0].Policy.MinPassBasisPoints = 5000
	options := board.DraftOptions{}
	if path == "ordinary derived draft" {
		options.FromBaseline = fx.approved.Descriptor.Digest
	} else {
		base.ID = "INDEPENDENT-ROSTER"
		base.Revision = 1
		base.Behaviors[0].Policy.Revision++
	}
	if policy == "different" {
		base.Behaviors[0].Policy.ID = "INDEPENDENT-POLICY-NAME"
	}
	draft, err := fx.app.CreateDraft(fx.reducer.actor, "pilot", base, options)
	if err != nil {
		t.Fatal(err)
	}
	if path == "copied/relabelled draft" {
		copyContents := draft.Baseline
		copyContents.ID = "COPIED-ROSTER"
		draft, err = fx.app.CreateDraft(fx.peer.actor, "pilot", copyContents, board.DraftOptions{FromDraft: draft.ID})
		if err != nil || draft.ParentDraft == "" {
			t.Fatalf("draft copy did not retain its lineage: %v", err)
		}
	}
	if draft.Baseline.Behaviors[0].Policy.MinPassBasisPoints != 5000 {
		t.Fatal("pending draft did not submit precisely the lowered threshold")
	}
	return draft
}

func (fx thresholdHTTPFixture) commitFailure(t *testing.T) board.Receipt {
	t.Helper()
	preview, err := fx.app.PreviewImport(fx.reducer.actor, "pilot", fx.old.ID, fx.adverse)
	if err != nil || preview.Token == "" || len(preview.Issues) != 0 {
		t.Fatalf("real failure fixture could not be staged: %v", err)
	}
	receipt, err := fx.app.CommitImport(fx.reducer.actor, "pilot", preview.Token, fx.adverse)
	if err != nil || receipt.LedgerHighWater != 1 || !slices.Equal(receipt.AddedRuns, []string{"REVIEW-FAIL"}) {
		t.Fatalf("required definitive failure did not commit exactly once: %v", err)
	}
	return receipt
}

func (fx thresholdHTTPFixture) endpoint(draft board.Draft) string {
	return "/p/pilot/drafts/" + draft.ID + "/approve"
}

func (fx thresholdHTTPFixture) submit(t *testing.T, user authoritySession, draft board.Draft) authorityResponse {
	t.Helper()
	return (authorityFixture{visit: fx.visit}).send(t, user, fx.endpoint(draft), url.Values{
		"revision": {strconv.FormatUint(draft.Revision, 10)},
	}, nil)
}

func (fx thresholdHTTPFixture) verifyDenial(t *testing.T, draft board.Draft, receipt board.Receipt, refused authorityResponse) {
	t.Helper()
	if refused.status != http.StatusForbidden || refused.location != "" {
		t.Fatalf("the reducing reviewer completed approval: HTTP %d location %q", refused.status, refused.location)
	}
	approved, _, err := fx.app.Baselines("reviewer", "pilot")
	if err != nil || len(approved) != 1 || approved[0].Descriptor != fx.approved.Descriptor {
		t.Fatalf("denied approval changed the approved catalog: %v", err)
	}
	kept, err := fx.app.Draft("reviewer", "pilot", draft.ID)
	if err != nil || !reflect.DeepEqual(kept, draft) || kept.ApprovedDigest != "" {
		t.Fatalf("denied approval changed the pending draft revision: %v", err)
	}
	fx.requirePreservedFailure(t, receipt)
}

func (fx thresholdHTTPFixture) requirePreservedFailure(t *testing.T, receipt board.Receipt) {
	t.Helper()
	evaluated, err := fx.app.Assess("reviewer", "pilot", fx.old.ID)
	if err != nil || evaluated.Status != board.Blocked || evaluated.LedgerHighWater != receipt.LedgerHighWater ||
		len(evaluated.Behaviors) != 1 || evaluated.Behaviors[0].Status != board.Fail ||
		evaluated.Behaviors[0].CurrentRunID != "REVIEW-FAIL" ||
		evaluated.Behaviors[0].Passed != 1 || evaluated.Behaviors[0].Planned != 2 {
		t.Fatalf("threshold review changed the old blocked decision: %v", err)
	}
	before, err := fx.app.SourceCSV("reviewer", "pilot", receipt.ID)
	if err != nil || !bytes.Equal(before, fx.adverse) {
		t.Fatalf("threshold review changed the original failed import: %v", err)
	}
}

func (fx thresholdHTTPFixture) approveIndependently(t *testing.T, draft board.Draft, receipt board.Receipt) {
	t.Helper()
	got := fx.submit(t, fx.peer, draft)
	if got.status != http.StatusSeeOther || got.location != strings.TrimSuffix(fx.endpoint(draft), "/approve") {
		t.Fatalf("independent reviewer could not approve the exact pending revision: HTTP %d", got.status)
	}
	retained, err := fx.app.Draft("reviewer", "pilot", draft.ID)
	if err != nil || retained.Revision != draft.Revision || retained.ApprovedDigest == "" {
		t.Fatalf("independent approval did not bind the exact requested draft: %v", err)
	}
	current, err := fx.app.Baseline("reviewer", "pilot", retained.ApprovedDigest)
	if err != nil || current.Approver != approvalPeer || len(current.Thresholds) != 1 ||
		current.Thresholds[0].Before != 10000 || current.Thresholds[0].After != 5000 ||
		current.Thresholds[0].CandidateID != fx.old.ID || current.Thresholds[0].BehaviorID != "BEH1" ||
		!slices.Equal(current.Thresholds[0].ReducingActors, []string{fx.reducer.actor}) {
		t.Fatalf("independent approval lost the D20 evidence and attribution: %#v %v", current, err)
	}
	fx.requirePreservedFailure(t, receipt)
}

func recordThresholdHTTPInstance(t *testing.T, id string, parameters map[string]any, actual string) {
	t.Helper()
	caseID, _, _ := strings.Cut(id, ".")
	wire, err := json.Marshal(map[string]any{"id": id, "case_id": caseID, "parameters": parameters, "actual": actual})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("REB_ACCEPTANCE_INSTANCE " + string(wire))
}

func TestFollowupD20DirectApprovalVariantsCannotResetFailure(t *testing.T) {
	for _, item := range []struct {
		id, path, policy, predecessor string
	}{
		{"AC-001-08.P005", "ordinary derived draft", "same", "present"},
		{"AC-001-08.P006", "standalone draft", "same", "none"},
		{"AC-001-08.P007", "ordinary derived draft", "different", "present"},
		{"AC-001-08.P008", "standalone draft", "different", "none"},
		{"AC-001-08.P009", "copied/relabelled draft", "different", "none"},
	} {
		t.Run(item.id, func(t *testing.T) {
			fx := newThresholdHTTPFixture(t)
			receipt := fx.commitFailure(t)
			draft := fx.pendingDraft(t, item.path, item.policy)
			same := draft.Baseline.Behaviors[0].Policy.ID == fx.approved.Baseline.Behaviors[0].Policy.ID
			if same != (item.policy == "same") || (draft.ParentBaseline != "") != (item.predecessor == "present") {
				t.Fatal("retained policy or baseline lineage differs from the requested scenario")
			}
			fx.verifyDenial(t, draft, receipt, fx.submit(t, fx.reducer, draft))
			fx.approveIndependently(t, draft, receipt)
			recordThresholdHTTPInstance(t, item.id, map[string]any{"creationPath": item.path, "policyIdRelation": item.policy,
				"predecessor": item.predecessor, "request": "direct endpoint"},
				"The server denies each attempt by the reducing actor. No approved revision or assessment changes")
		})
	}
}

func TestFollowupD20FailureCommitsDuringApprovalRequest(t *testing.T) {
	fx := newThresholdHTTPFixture(t)
	pending := fx.pendingDraft(t, "ordinary derived draft", "same")
	earlier, err := fx.app.ReviewThresholds(fx.reducer.actor, "pilot", pending.ID)
	if err != nil || len(earlier) != 0 {
		t.Fatalf("setup prematurely required independent review: %v", err)
	}

	form := url.Values{"_csrf": {fx.reducer.csrf}, "revision": {strconv.FormatUint(pending.Revision, 10)}}.Encode()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	headersAccepted := make(chan struct{}, 1)
	trace := &httptrace.ClientTrace{Got100Continue: func() { headersAccepted <- struct{}{} }}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, fx.visit+fx.endpoint(pending), reader)
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = int64(len(form))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Expect", "100-continue")

	type answer struct {
		response authorityResponse
		err      error
	}
	finished := make(chan answer, 1)
	go func() {
		res, err := fx.reducer.client.Do(request)
		if err != nil {
			finished <- answer{err: err}
			return
		}
		body, readError := io.ReadAll(res.Body)
		closeError := res.Body.Close()
		if readError == nil {
			readError = closeError
		}
		finished <- answer{authorityResponse{status: res.StatusCode, body: string(body), location: res.Header.Get("Location")}, readError}
	}()

	select {
	case <-headersAccepted:
		// The actual HTTP server has reached the first read of the valid
		// approval form body. The producer has supplied none of that body yet.
	case early := <-finished:
		t.Fatalf("approval completed before its body was sent: %#v", early)
	case <-ctx.Done():
		t.Fatal("server never acknowledged the in-flight authenticated approval")
	}
	receipt := fx.commitFailure(t)
	select {
	case early := <-finished:
		t.Fatalf("approval returned before the committed failure and body release: %#v", early)
	default:
	}
	if _, err := io.WriteString(writer, form); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case result := <-finished:
		if result.err != nil {
			t.Fatal(result.err)
		}
		fx.verifyDenial(t, pending, receipt, result.response)
	case <-ctx.Done():
		t.Fatal("approval did not return after its request body was released")
	}
	fx.approveIndependently(t, pending, receipt)
	recordThresholdHTTPInstance(t, "AC-002-20.P001", map[string]any{},
		"The approval-time check sees the committed failure and requires a different reviewer")
}
