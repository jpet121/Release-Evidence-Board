package browser_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"release-evidence-board/internal/board"
	"release-evidence-board/internal/browser"
)

func TestFollowupLostHTTPCommitResponseReplaysOriginalAfterRestart(t *testing.T) {
	const contributor = "replay-contributor"
	settings := board.Settings{ProjectID: "pilot", ProductID: "DEMO"}
	database := filepath.Join(t.TempDir(), "response-loss.db")
	app, err := board.Open(database, settings)
	if err != nil {
		t.Fatal(err)
	}
	closeApp := closeResponseLossBoard(t, app)
	if err := app.Bootstrap("reviewer", browserPassword); err != nil {
		t.Fatal(err)
	}
	if err := app.SetAccount("reviewer", contributor, browserPassword, []board.Role{board.Contributor}); err != nil {
		t.Fatal(err)
	}
	draft, err := app.CreateDraft("reviewer", "pilot", board.Baseline{
		ProductID: "DEMO", ID: "REPLAY", Revision: 1, Behaviors: []board.Behavior{{
			ID: "BEH1", Revision: 1, Required: true, Text: "Exact protected release record",
			Policy: board.Policy{ID: "P1", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
			Tests:  []board.TestPlan{{ID: "T1", ExpectedOutcome: "Authorized response", MustPass: true, PlannedAttempts: 1}},
		}},
	}, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := app.Approve("reviewer", "pilot", draft.ID, draft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	candidate, reused, err := app.CreateCandidate(contributor, "pilot", board.CandidateInput{BaselineDigest: approved.Descriptor.Digest, Manifest: board.Manifest{
		ProductID: "DEMO", ReleaseID: "R1", ApplicationVersion: "1.0", BuildDigest: strings.Repeat("a", 64), EnvironmentID: "E1",
		PromptDigest: "not_applicable", ModelConfigDigest: "not_applicable", KnowledgeDigest: "not_applicable",
	}})
	if err != nil || reused {
		t.Fatalf("response-loss fixture did not create its own candidate: %v", err)
	}
	start := func(app *board.Board) (authorityFixture, authoritySession, func()) {
		ui := browser.New(app, false)
		native := httptest.NewServer(ui)
		stop := sync.OnceFunc(func() {
			native.Close()
			ui.Close()
		})
		t.Cleanup(stop)
		client, csrf := loginSession(t, native.URL, contributor)
		client.Timeout = 5 * time.Second
		return authorityFixture{app: app, visit: native.URL}, authoritySession{contributor, client, csrf}, stop
	}
	first, submitter, stopFirst := start(app)
	stamp := board.FormatUTC(time.Now().UTC().Add(-time.Minute))
	raw := followupBrowserCSV(t, followupBrowserResult(candidate, "R1", "BEH1", "P1", "T1", "REPLAY-RUN", "REPLAY-RESULT", stamp, "PASS"))
	base := "/p/pilot/candidates/" + candidate.ID
	digest := fixtureHash(raw)
	wantReceipt := "I-" + digest
	wantLocation := "/p/pilot/receipts/" + wantReceipt
	preview := first.send(t, submitter, base+"/preview", nil, raw)
	previewToken := authorityPreviewToken.FindStringSubmatch(preview.body)
	previewRevision := authorityPreviewRevision.FindStringSubmatch(preview.body)
	if preview.status != http.StatusOK || len(previewToken) != 2 || len(previewRevision) != 2 || !strings.Contains(preview.body, digest) {
		t.Fatalf("exact preview must precede the interrupted response: HTTP %d", preview.status)
	}
	revision, err := strconv.ParseUint(previewRevision[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	cut := dropRealCommitResponse(t, first.visit, base+"/commit", wantLocation)
	reply, err := submitter.client.PostForm(cut.URL+base+"/commit", url.Values{"preview": {previewToken[1]}, "_csrf": {submitter.csrf}})
	if reply != nil {
		if closeErr := reply.Body.Close(); closeErr != nil {
			t.Logf("closing the unexpected delivered response: %v", closeErr)
		}
	}
	if reply != nil || (!errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF)) {
		t.Fatalf("the original authenticated client must lose its socket before any response: %v", err)
	}
	cut.join(t)
	t.Log("Client received EOF after actual upstream 303 with the exact committed receipt location")
	stopFirst()
	closeApp()

	restored, err := board.Open(database, settings)
	if err != nil {
		t.Fatal(err)
	}
	closeResponseLossBoard(t, restored)
	second, retrying, _ := start(restored)
	firstReceipt, err := restored.Receipt(contributor, "pilot", wantReceipt)
	if err != nil || firstReceipt.Hash != digest || firstReceipt.Uploader != contributor || firstReceipt.LedgerHighWater != 1 ||
		!slices.Equal(firstReceipt.AddedRuns, []string{"REPLAY-RUN"}) || firstReceipt.Rows != 1 {
		t.Fatalf("origin success was not durable under the identical receipt after restart: %v", err)
	}
	fresh, err := restored.PreviewImport(contributor, "pilot", candidate.ID, raw)
	if err != nil || len(fresh.Issues) != 0 || fresh.Token == "" || fresh.Revision != revision+1 || fresh.ExistingReceiptID != wantReceipt || fresh.Hash != digest {
		t.Fatalf("original preview was not stale with exact committed bytes: %v", err)
	}
	if err := restored.CancelPreview(contributor, "pilot", fresh.Token); err != nil {
		t.Fatal(err)
	}
	before := second.audit(t)
	matching, writes := 0, 0
	for _, event := range before {
		if event.Action == "import.commit" && event.Outcome == "success" {
			writes++
			if event.Actor == contributor && slices.Contains(event.References, wantReceipt) {
				matching++
			}
		}
	}
	if matching != 1 || writes != 1 {
		t.Fatalf("restart found %d import writes and %d attributed to the lost reply", writes, matching)
	}
	result := second.send(t, retrying, base+"/commit", url.Values{"preview": {previewToken[1]}}, nil)
	if result.status != http.StatusSeeOther || result.location != wantLocation {
		t.Fatalf("exact stale preview replay did not return original receipt: HTTP %d", result.status)
	}
	replayed, err := restored.Receipt(contributor, "pilot", wantReceipt)
	if err != nil || !reflect.DeepEqual(replayed, firstReceipt) || !reflect.DeepEqual(second.audit(t), before) {
		t.Fatalf("retry changed immutable receipt metadata or material audit: %v", err)
	}
	stored, err := restored.SourceCSV(contributor, "pilot", wantReceipt)
	if err != nil || !bytes.Equal(stored, raw) {
		t.Fatalf("retry no longer points to the exact originally previewed and committed bytes: %v", err)
	}
	decision, err := restored.Assess(contributor, "pilot", candidate.ID)
	if err != nil || decision.Status != "SUFFICIENT" || decision.LedgerHighWater != 1 || len(decision.Behaviors) != 1 || len(decision.Behaviors[0].Runs) != 1 {
		t.Fatalf("replay changed committed rows, runs, or ledger sequences: %v", err)
	}
	run := decision.Behaviors[0].Runs[0]
	if run.ID != "REPLAY-RUN" || run.Sequence != 1 || len(run.Rows) != 1 || run.Rows[0].ResultID() != "REPLAY-RESULT" {
		t.Fatal("retry failed exact committed result readback")
	}
	if !t.Failed() {
		recordThresholdHTTPInstance(t, "AC-005-13.P001", map[string]any{}, "Return the original receipt. The retry creates no new rows, runs, or ledger sequences")
	}
}

func closeResponseLossBoard(t *testing.T, app *board.Board) func() {
	t.Helper()
	closeApp := sync.OnceFunc(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(closeApp)
	return closeApp
}

type cutCommitResponse struct {
	*httptest.Server
	observed chan error
	requests *atomic.Uint32
}

// Forward ordinary bytes to the actual application. On success, the TCP
// client connection is closed without writing the original response.
func dropRealCommitResponse(t *testing.T, origin, path, location string) cutCommitResponse {
	t.Helper()
	observed := make(chan error, 1)
	requests := &atomic.Uint32{}
	upstreamClient := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(upstreamClient.CloseIdleConnections)
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, incoming *http.Request) {
		if requests.Add(1) != 1 {
			http.Error(w, "local response-drop fixture received a repeat", http.StatusConflict)
			return
		}
		forward := func() error {
			if incoming.Method != http.MethodPost || incoming.URL.RequestURI() != path {
				return fmt.Errorf("unexpected proxy request %s %s", incoming.Method, incoming.URL.RequestURI())
			}
			req, err := http.NewRequestWithContext(incoming.Context(), incoming.Method, origin+path, incoming.Body)
			if err != nil {
				return err
			}
			req.Header, req.ContentLength = incoming.Header.Clone(), incoming.ContentLength
			reply, err := upstreamClient.Do(req)
			if err != nil {
				return err
			}
			_, discardErr := io.Copy(io.Discard, reply.Body)
			if err := errors.Join(discardErr, reply.Body.Close()); err != nil {
				return err
			}
			if reply.StatusCode != http.StatusSeeOther || reply.Header.Get("Location") != location {
				return fmt.Errorf("origin did not commit the exact receipt: HTTP %d", reply.StatusCode)
			}
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				return errors.New("owned HTTP/1.1 connection cannot be hijacked")
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				return err
			}
			return conn.Close()
		}
		err := forward()
		observed <- err
		if err != nil {
			http.Error(w, "local response-drop fixture failed", http.StatusBadGateway)
		}
	}))
	t.Cleanup(listener.Close)
	return cutCommitResponse{listener, observed, requests}
}

func (cut cutCommitResponse) join(t *testing.T) {
	t.Helper()
	cut.Close()
	select {
	case err := <-cut.observed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("controlled first request never completed")
	}
	if cut.requests.Load() != 1 {
		t.Fatal("the first authenticated HTTP request was repeated")
	}
}
