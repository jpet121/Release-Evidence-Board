package browser_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"release-evidence-board/internal/board"
	"release-evidence-board/internal/browser"
)

func TestHTTPConsumesConfiguredImportCountAndWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rate.db")
	id := seedHistoricalAI(t, path, time.Now().UTC(), nil)
	store, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	// An independently written first-party policy record isolates the HTTP
	// consumer from whether the separate Operator command supports writing it.
	raw := []byte(`{"count":2,"window":2000000000}`)
	updateErr := store.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		if err := meta.Put([]byte("import_rate_policy"), raw); err != nil {
			return err
		}
		return meta.Put([]byte("import_rate_policy_sha256"), []byte(fixtureHash(raw)))
	})
	if err := errors.Join(updateErr, store.Close()); err != nil {
		t.Fatal(err)
	}
	app, err := board.Open(path, board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := app.SetAccount("reviewer", "contributor", browserPassword, []board.Role{board.Contributor}); err != nil {
		t.Fatal(err)
	}
	handler := browser.New(app, false)
	t.Cleanup(handler.Close)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, csrf := loginSession(t, server.URL, "contributor")
	viewer, viewerCSRF := loginSession(t, server.URL, "viewer")
	root := server.URL + "/p/pilot/candidates/" + id
	if _, status := post(t, viewer, root+"/preview", url.Values{"_csrf": {viewerCSRF}}); status != http.StatusForbidden {
		t.Fatalf("Viewer request was not denied before the import gate: %d", status)
	}
	for _, request := range []struct {
		route string
		want  int
	}{{"/preview", http.StatusUnprocessableEntity}, {"/commit", http.StatusNotFound}} {
		if _, status := post(t, client, root+request.route, url.Values{"_csrf": {csrf}}); status != request.want {
			t.Fatalf("allowed quota request did not reach validation: %s = %d", request.route, status)
		}
	}
	resp, err := client.PostForm(root+"/preview", url.Values{"_csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "2" {
		t.Fatalf("configured third request: HTTP %d Retry-After %q, want 429 and two seconds", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	timer := time.NewTimer(2100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	case <-timer.C:
	}
	if _, status := post(t, client, root+"/preview", url.Values{"_csrf": {csrf}}); status != http.StatusUnprocessableEntity {
		t.Fatalf("configured rolling window did not reopen quota: HTTP %d", status)
	}
}

func TestImportRateChangePreservesExactCommittedRetry(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "rate-revision.db")
	id := seedHistoricalAI(t, path, now, nil)
	app, err := board.Open(path, board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	candidate, _, _, err := app.Candidate("reviewer", "pilot", id)
	if err != nil {
		t.Fatal(err)
	}
	bind := func(key string) ([]byte, board.Preview) {
		t.Helper()
		at := board.FormatUTC(now)
		raw := []byte(followupHeader + fmt.Sprintf("reb-results/1.3,RESULT,RESULT-%s,DEMO,HISTORY,%s,%s,HISTORY,1,P-HISTORY,1,T-HISTORY,RUN-%s,1,%s,%s,PASS,,\n",
			key, candidate.ManifestDigest, candidate.BaselineDigest, key, at, at))
		preview, err := app.PreviewImport("reviewer", "pilot", id, raw)
		if err != nil {
			t.Fatal(err)
		}
		return raw, preview
	}
	raw, committed := bind("COMMITTED")
	receipt, err := app.CommitImport("reviewer", "pilot", committed.Token, raw)
	if err != nil {
		t.Fatal(err)
	}
	newRaw, pending := bind("PENDING")
	scope := []string{id}
	stale, err := app.PreviewPurge("reviewer", "pilot", scope)
	if err != nil {
		t.Fatal(err)
	}
	policy := board.ImportRatePolicy{Count: 4, Window: 2 * time.Minute}
	now = now.Add(time.Second)
	if err := app.SetImportRate("reviewer", "pilot", policy); err != nil {
		t.Fatal(err)
	}
	if _, err := app.CommitImport("reviewer", "pilot", pending.Token, newRaw); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("rate update kept stale import preview valid: %v", err)
	}
	replay, err := app.CommitImport("reviewer", "pilot", committed.Token, raw)
	if err != nil || !reflect.DeepEqual(replay, receipt) {
		t.Fatalf("rate update invalidated exact committed receipt: %v", err)
	}
	if _, err := app.Purge("reviewer", "pilot", scope, stale.Confirmation); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("rate update kept stale purge preview valid: %v", err)
	}
	fresh, err := app.PreviewPurge("reviewer", "pilot", scope)
	if err != nil || fresh.Revision != stale.Revision+1 {
		t.Fatalf("rate change did not advance exactly one data revision: %d -> %d, %v", stale.Revision, fresh.Revision, err)
	}
	if err := app.SetImportRate("reviewer", "pilot", policy); err != nil {
		t.Fatal(err)
	}
	unchanged, err := app.PreviewPurge("reviewer", "pilot", scope)
	if err != nil || unchanged.Confirmation != fresh.Confirmation {
		t.Fatalf("identical rate policy invalidated the fresh purge preview: %v", err)
	}
}
