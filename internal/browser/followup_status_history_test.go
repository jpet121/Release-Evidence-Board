package browser_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"release-evidence-board/internal/board"
	"release-evidence-board/internal/browser"
)

func TestBrowserHealthShowsAvailableBudgetWithoutARecordedReview(t *testing.T) {
	now := interruptionFixtureTime
	path := filepath.Join(t.TempDir(), "prior.db")
	seedHistoricalAI(t, path, now, func(item map[string]any) { item["reviews"] = []any{} })
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mutateArchiveFixture(t, path, original, editArchiveRecord("ai_budget_v1", func(item map[string]any) {
		item["amount_microusd"] = 1_000_000
	}))
	cfg := board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return now }}
	app, err := board.Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	backup, err := app.Backup("reviewer", "pilot", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	restored, err := board.RestoreBackup(backup.Path, filepath.Join(t.TempDir(), "drill.db"), cfg)
	if err != nil || !restored.IntegrityOK {
		t.Fatalf("independent restore drill input failed verification: %+v, %v", restored, err)
	}
	if err := app.RecordRestoreDrill("reviewer", "pilot", backup.Path); err != nil {
		t.Fatal(err)
	}
	visit := serveBrowserTestBoard(t, app)
	client, _ := loginSession(t, visit, "reviewer")
	page, status := response(t, client.Get, visit+"/p/pilot/health")
	if status != http.StatusOK || !strings.Contains(page, "Budget available") || !strings.Contains(page, "REQ-SYNTHETIC-1") ||
		!strings.Contains(page, "4000000") || strings.Contains(page, "role=\"alert\"") ||
		strings.Contains(page, "PRIVATE-SYNTHETIC") || strings.Contains(page, "S-1 · accept") {
		t.Fatalf("healthy AI-off summary falsely showed a review, charge cap or health alert: HTTP %d", status)
	}
}

func TestBrowserHealthDisclosesUnavailableChecksThroughActualStorage(t *testing.T) {
	now := interruptionFixtureTime
	path := filepath.Join(t.TempDir(), "unavailable.db")
	seedHistoricalAI(t, path, now, nil)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mutateArchiveFixture(t, path, original, func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("meta")).Put([]byte("ledger"), []byte("not-a-ledger"))
	})
	app, err := board.Open(path, board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	for i := 1; i <= 3; i++ {
		check := app.Check()
		if check.DatabaseOK || check.ConsecutiveFailures != i {
			t.Fatalf("storage availability fixture check %d = %+v", i, check)
		}
	}
	visit := serveBrowserTestBoard(t, app)
	client, _ := loginSession(t, visit, "reviewer")
	page, status := response(t, client.Get, visit+"/p/pilot/health")
	if status != http.StatusOK || !strings.Contains(page, "<dt>Database</dt><dd>Unavailable</dd>") ||
		!strings.Contains(page, "Three consecutive application health checks failed.") ||
		strings.Contains(page, "not-a-ledger") {
		t.Fatalf("actual failed storage checks were concealed or leaked raw data: HTTP %d", status)
	}
}

func TestBrowserRetainedSuccessorLinksShowPurgedPredecessorAndEmptyAIHistory(t *testing.T) {
	now := interruptionFixtureTime
	path := filepath.Join(t.TempDir(), "lineage.db")
	previous := seedHistoricalAI(t, path, now, nil)
	now = now.Add(180 * 24 * time.Hour)
	app, err := board.Open(path, board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	old, manifest, _, err := app.Candidate("reviewer", "pilot", previous)
	if err != nil {
		t.Fatal(err)
	}
	successorManifest := manifest.Manifest
	successorManifest.ReleaseID = "CURRENT"
	successorManifest.EnvironmentID = "CURRENT-ENV"
	successor, reused, err := app.CreateCandidate("reviewer", "pilot", board.CandidateInput{
		Manifest: successorManifest, BaselineDigest: old.BaselineDigest,
		PredecessorID: previous, Correction: "Recorded synthetic environment correction",
	})
	if err != nil || reused {
		t.Fatalf("retained successor fixture did not create a distinct valid candidate: %v", err)
	}
	visit := serveBrowserTestBoard(t, app)
	client, _ := loginSession(t, visit, "reviewer")
	target := visit + "/p/pilot/candidates/" + successor.ID
	candidate, status := response(t, client.Get, target)
	if status != http.StatusOK || !strings.Contains(candidate, "href=\"/p/pilot/candidates/"+previous+"\">") {
		t.Fatalf("available predecessor was not linked: HTTP %d", status)
	}
	plan, err := app.PreviewPurge("reviewer", "pilot", []string{previous})
	if err != nil || len(plan.AIRequestIDs) != 1 {
		t.Fatalf("expired predecessor did not enter selected purge: %+v, %v", plan, err)
	}
	if _, err := app.Purge("reviewer", "pilot", []string{previous}, plan.Confirmation); err != nil {
		t.Fatal(err)
	}
	candidate, status = response(t, client.Get, target)
	if status != http.StatusOK || !strings.Contains(candidate, previous+" · purged record") ||
		strings.Contains(candidate, "href=\"/p/pilot/candidates/"+previous+"\">") || !strings.Contains(candidate, "MISSING_RUN") {
		t.Fatalf("retained successor invented a live prior link or gained evidence: HTTP %d", status)
	}
	health, status := response(t, client.Get, visit+"/p/pilot/health")
	if status != http.StatusOK || !strings.Contains(health, "Retained AI reviews") || !strings.Contains(health, "Budget exhausted") ||
		!strings.Contains(health, "5000000") || strings.Contains(health, "REQ-SYNTHETIC-1") ||
		strings.Contains(health, historyPayloadSHA) || strings.Contains(health, historyResponseSHA) {
		t.Fatalf("purged private history appeared in the retained summary or lost its separate unresolved hold: HTTP %d", status)
	}
}

func TestBrowserCorruptCollectionsHoldFreshActionsAndNeverServeAlteredBytes(t *testing.T) {
	now := interruptionFixtureTime
	path := filepath.Join(t.TempDir(), "collections.db")
	id := seedHistoricalAI(t, path, now, nil)
	cfg := board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return now }}
	source, err := board.Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	candidate, _, _, err := source.Candidate("reviewer", "pilot", id)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := source.CreateDraft("reviewer", "pilot", board.Baseline{ProductID: "DEMO", ID: "REVIEWABLE"}, board.DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	report, err := source.Freeze("reviewer", "pilot", id)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const changed = "UNBOUND SYNTHETIC DATA"
	for _, tc := range []struct {
		bucket, key string
		routes      []string
	}{
		{"baseline_metadata_sha256", candidate.BaselineDigest, []string{"/p/pilot/baselines", "/p/pilot/candidates/new"}},
		{"candidate_metadata_sha256", id, []string{"/p/pilot", "/p/pilot/candidates/new", "/p/pilot/drafts/" + draft.ID}},
		{"report_html", report.ID, []string{"/p/pilot/candidates/" + id + "/reports", "/p/pilot/reports/" + report.ID + "?download=1"}},
	} {
		t.Run(tc.bucket, func(t *testing.T) {
			bad := filepath.Join(t.TempDir(), "bad.db")
			mutateArchiveFixture(t, bad, original, func(tx *bolt.Tx) error {
				return tx.Bucket([]byte(tc.bucket)).Put([]byte(tc.key), []byte(changed))
			})
			destination := filepath.Join(t.TempDir(), "restore.db")
			restored, err := board.RestoreBackup(bad, destination, cfg)
			if err != nil || restored.IntegrityOK {
				t.Fatalf("unbound collection verified correctly checksummed transport: %+v, %v", restored, err)
			}
			app, err := board.Open(destination, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := app.Close(); err != nil {
					t.Error(err)
				}
			})
			visit := serveBrowserTestBoard(t, app)
			client := browserTestSession(t)
			page, status := response(t, client.Get, visit+"/login")
			token := boundToken.FindStringSubmatch(page)
			if status != http.StatusOK || len(token) != 2 {
				t.Fatalf("protected retention hold prevented normal assigned authentication: HTTP %d", status)
			}
			if _, status := post(t, client, visit+"/login", url.Values{"actor": {"reviewer"}, "password": {browserPassword}, "_csrf": {token[1]}}); status != http.StatusSeeOther {
				t.Fatalf("owner could not sign in to inspect corrupted storage: HTTP %d", status)
			}
			for _, route := range tc.routes {
				body, status := response(t, client.Get, visit+route)
				if status != http.StatusServiceUnavailable || strings.Contains(body, changed) ||
					!strings.Contains(body, "Evidence integrity requires operator verification") {
					t.Errorf("corrupt %s disclosed or concealed affected stored data at %s: HTTP %d", tc.bucket, route, status)
				}
			}
		})
	}
}

func TestBrowserRestoredAccountWithoutRolesCannotReadWorkspace(t *testing.T) {
	now := interruptionFixtureTime
	path := filepath.Join(t.TempDir(), "missing-role.db")
	seedHistoricalAI(t, path, now, nil)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mutateArchiveFixture(t, path, original, func(tx *bolt.Tx) error {
		accounts := tx.Bucket([]byte("accounts"))
		id := []byte("viewer")
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(accounts.Get(id), &entry); err != nil {
			return err
		}
		entry["Roles"] = json.RawMessage(`[]`)
		raw, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		return accounts.Put(id, raw)
	})
	cfg := board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return now }}
	dest := filepath.Join(t.TempDir(), "restored.db")
	if _, err := board.RestoreBackup(path, dest, cfg); err != nil {
		t.Fatal(err)
	}
	app, err := board.Open(dest, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	account, err := app.Account("viewer")
	if err != nil || len(account.Roles) != 0 {
		t.Fatalf("fixture did not retain the lack of authorization assignments: %v", err)
	}
	visit := serveBrowserTestBoard(t, app)
	client := browserTestSession(t)
	page, status := response(t, client.Get, visit+"/login")
	token := boundToken.FindStringSubmatch(page)
	if status != http.StatusOK || len(token) != 2 {
		t.Fatalf("authentication form was unavailable: HTTP %d", status)
	}
	if _, status := post(t, client, visit+"/login", url.Values{
		"actor": {"viewer"}, "password": {browserPassword}, "_csrf": {token[1]},
	}); status != http.StatusSeeOther {
		t.Fatalf("valid credentials did not reach the separate authorization boundary: HTTP %d", status)
	}
	body, status := response(t, client.Get, visit+"/p/pilot")
	if status != http.StatusForbidden || strings.Contains(body, "Active candidates") ||
		strings.Contains(body, "REQ-SYNTHETIC-1") || strings.Contains(body, "Release candidates") {
		t.Fatalf("workspace granted an unassigned restored account access: HTTP %d", status)
	}
}

func serveBrowserTestBoard(t *testing.T, app *board.Board) string {
	t.Helper()
	handler := browser.New(app, false)
	t.Cleanup(handler.Close)
	web := httptest.NewServer(handler)
	t.Cleanup(web.Close)
	return web.URL
}
