package browser_test

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupEvidenceDetailSurvivesUnavailableExternalSource(t *testing.T) {
	app, visit, id := startTestBrowser(t)
	viewer, _ := loginSession(t, visit, "viewer")
	candidate, _, _, err := app.Candidate("reviewer", "pilot", id)
	if err != nil {
		t.Fatal(err)
	}
	var connections atomic.Int32
	unavailable := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	unavailable.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	unavailable.StartTLS()
	t.Cleanup(unavailable.Close)
	probe, err := unavailable.Client().Get(unavailable.URL + "/unarchived")
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if probe.StatusCode != http.StatusServiceUnavailable || connections.Load() != 1 {
		t.Fatal("synthetic external reference was not observed unavailable")
	}
	at := time.Now().UTC()
	long := strings.Repeat("é", 4000)
	markup := `<strong data-fixture-marker="literal">keep these bytes</strong>`
	old := followupBrowserResult(candidate, "R1", "BEH1", "P1", "T1", "OLD-DECISIVE", "OLDER-RESULT", board.FormatUTC(at.Add(-5*time.Minute)), "FAIL")
	newest := followupBrowserResult(candidate, "R1", "BEH1", "P1", "T1", "NEW-CURRENT", "LATER-RESULT", board.FormatUTC(at.Add(-2*time.Minute)), "PASS")
	old[17], old[18], newest[18] = unavailable.URL+"/unarchived", long, markup
	raw := followupBrowserCSV(t, newest, old)
	receipt := commitFollowupBrowserCSV(t, app, id, raw)
	path := visit + "/p/pilot/candidates/" + id + "/behaviors?id=BEH1"
	detail, status := response(t, viewer.Get, path)
	if status != http.StatusOK {
		t.Fatalf("retained details returned HTTP %d", status)
	}
	for _, value := range []string{
		candidate.ID, candidate.ManifestDigest, candidate.BaselineDigest, "Policy P1 revision 1 · 10000 basis points · 24 hours",
		"Current run <strong>NEW-CURRENT</strong>", "OLD-DECISIVE", "MUST_PASS_FAILED", "Definitive failure",
		"OLDER-RESULT", "LATER-RESULT", receipt.Hash, "Unarchived source link", unavailable.URL + "/unarchived",
		long, "&lt;strong data-fixture-marker=", "keep these bytes&lt;/strong&gt;",
	} {
		if !strings.Contains(detail, value) {
			if value == long {
				t.Fatal("maximum-length retained source excerpt was shortened or changed")
			}
			t.Fatalf("retained details omitted %q", value)
		}
	}
	if strings.Contains(detail, markup) || strings.Contains(strings.ToLower(detail), "source verified") {
		t.Fatal("view interpreted literal markup or asserted unavailable source verification")
	}
	protected, _, status := readFollowupArtifact(t, viewer, visit+"/p/pilot/receipts/"+receipt.ID+"/source")
	if status != http.StatusOK || !bytes.Equal(protected, raw) || connections.Load() != 1 {
		t.Fatal("local evidence depended on the external source or changed exact bytes")
	}
	t.Log("retained current and failure anchors visible, local raw CSV exact, synthetic HTTPS 503 stayed unarchived with no extra connection")
	recordCorroboratedBrowserObservation(t)
}

func TestFollowupCandidateViewsKeepDistinctStatusDuringOverlappingHTTPReads(t *testing.T) {
	app, visit, id := startTestBrowser(t)
	viewer, _ := loginSession(t, visit, "viewer")
	first, stored, _, err := app.Candidate("reviewer", "pilot", id)
	if err != nil {
		t.Fatal(err)
	}
	manifest := stored.Manifest
	manifest.BuildDigest = strings.Repeat("b", 64)
	second, reused, err := app.CreateCandidate("reviewer", "pilot", board.CandidateInput{
		Manifest: manifest, BaselineDigest: first.BaselineDigest, DisplayVersion: first.DisplayVersion,
	})
	if err != nil || reused || second.DisplayVersion != first.DisplayVersion {
		t.Fatalf("could not distinguish same-label candidates: %v", err)
	}
	at := board.FormatUTC(time.Now().UTC().Add(-time.Minute))
	commitFollowupBrowserCSV(t, app, first.ID, followupBrowserCSV(t,
		followupBrowserResult(first, "R1", "BEH1", "P1", "T1", "OWN-PASS", "PASS-RESULT", at, "PASS")))
	type result struct {
		page *http.Response
		err  error
	}
	start := make(chan struct{})
	firstResponse, secondResponse := make(chan result, 1), make(chan result, 1)
	for _, read := range []struct {
		id  string
		out chan result
	}{{first.ID, firstResponse}, {second.ID, secondResponse}} {
		go func() {
			<-start
			page, err := viewer.Get(visit + "/p/pilot/candidates/" + read.id)
			read.out <- result{page, err}
		}()
	}
	close(start)
	left, right := <-firstResponse, <-secondResponse
	for _, data := range []struct {
		result    result
		id, other string
		want      string
	}{{left, first.ID, second.ID, "SUFFICIENT"}, {right, second.ID, first.ID, "BLOCKED"}} {
		body, status := response(t, func(string) (*http.Response, error) {
			return data.result.page, data.result.err
		}, visit)
		summary := strings.SplitN(body, "<details", 2)[0]
		if status != http.StatusOK || !strings.Contains(summary, "<code>"+data.id+"</code>") ||
			strings.Contains(summary, data.other) || !strings.Contains(summary, `class="status `+data.want+`"`) {
			t.Fatal("HTTP decision response combined identities across concurrent same-label views")
		}
	}
}

func TestFollowupRunHistoryAndDecisionHaveNoPublicOverrideOperation(t *testing.T) {
	fx := newAuthorityFixture(t)
	candidate, _, _, err := fx.app.Candidate("reviewer", "pilot", fx.candidateID)
	if err != nil {
		t.Fatal(err)
	}
	stamp := board.FormatUTC(time.Now().UTC().Add(-time.Minute))
	commitFollowupBrowserCSV(t, fx.app, candidate.ID, followupBrowserCSV(t,
		followupBrowserResult(candidate, "R1", "BEH1", "P1", "T1", "RETAINED-FAIL", "BLOCKER", stamp, "FAIL")))
	base := "/p/pilot/candidates/" + candidate.ID
	before := fx.state(t)
	for _, path := range []string{"/runs/select", "/runs/delete", "/runs/reorder", "/runs/invalidate", "/override", "/deploy"} {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			attempt := fx.send(t, fx.reviewer, base+path, nil, nil)
			if attempt.status != http.StatusNotFound && attempt.status != http.StatusMethodNotAllowed && attempt.status != http.StatusForbidden {
				t.Fatalf("unsupported authority request returned HTTP %d", attempt.status)
			}
			if after := fx.state(t); !reflect.DeepEqual(after, before) || after.status != "BLOCKED" || after.ledger != 1 {
				t.Fatal("run history or retained decision changed")
			}
			recordCorroboratedBrowserObservation(t)
		})
	}
}
