package browser_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

const historyPayload = `{"source":"behavior:HISTORY:1","instructions":"PRIVATE-SYNTHETIC-REQUEST"}`
const historyResponse = `{"suggestions":[{"id":"S-1","behavior_id":"HISTORY","source_row_ids":["behavior:HISTORY:1"],"proposed_test":"PRIVATE-SYNTHETIC-RESPONSE","rationale":"Candidate behavior example","uncertainty":"clear","question":""}],"groups":[]}`
const historyPayloadSHA = "b90e9832dd18415e9b500193f5821e188125bedb4bd4baccff23aa0b72b60219"
const historyResponseSHA = "6f3ebd0301703d7e403c782ca20b588094e66d6fccede226914f76852fb11941"

func fixtureHash(raw []byte) string {
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

// The test writes the documented snapshot format independently of production
// decoders. Application-owned candidate and audit records use public operations.
func seedHistoricalAI(t *testing.T, path string, now time.Time, alter func(map[string]any)) string {
	t.Helper()
	created := now.Add(-5 * time.Minute).Truncate(time.Microsecond)
	app, err := board.Open(path, board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return created }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := app.Bootstrap("reviewer", browserPassword); err != nil {
		t.Fatal(err)
	}
	if err := app.SetAccount("reviewer", "viewer", browserPassword, []board.Role{board.Viewer}); err != nil {
		t.Fatal(err)
	}
	draft, err := app.CreateDraft("reviewer", "pilot", board.Baseline{
		ProductID: "DEMO", ID: "AI-HISTORY-BASE", Revision: 1,
		Behaviors: []board.Behavior{{
			ID: "HISTORY", Revision: 1, Text: "A retained AI sample is available for authorized review.", Required: true,
			Policy: board.Policy{ID: "P-HISTORY", Revision: 1, MaxAgeHours: 24, MinPassBasisPoints: 10000},
			Tests:  []board.TestPlan{{ID: "T-HISTORY", ExpectedOutcome: "Synthetic history fixture works", PlannedAttempts: 1, MustPass: true}},
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
		Manifest: board.Manifest{ProductID: "DEMO", ReleaseID: "HISTORY", ApplicationVersion: "1",
			BuildDigest: strings.Repeat("7", 64), PromptDigest: "not_applicable",
			ModelConfigDigest: "not_applicable", KnowledgeDigest: "not_applicable", EnvironmentID: "AI-ARCHIVE"},
	})
	if err != nil || reused {
		t.Fatalf("create candidate for prior synthetic history: reused=%v err=%v", reused, err)
	}
	stamp := board.FormatUTC(created)
	original := []byte(followupHeader + fmt.Sprintf("reb-results/1.3,RESULT,RES-HISTORY,DEMO,HISTORY,%s,%s,HISTORY,1,P-HISTORY,1,T-HISTORY,RUN-HISTORY,1,%s,%s,PASS,,\n",
		candidate.ManifestDigest, candidate.BaselineDigest, stamp, stamp))
	preview, err := app.PreviewImport("reviewer", "pilot", candidate.ID, original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.CommitImport("reviewer", "pilot", preview.Token, original); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}

	requested := board.FormatUTC(created.Add(time.Minute))
	history := map[string]any{
		"candidate_id": candidate.ID, "requester": "reviewer", "requested_at": requested,
		"completed_at": board.FormatUTC(created.Add(2 * time.Minute)),
		"disabled_at":  board.FormatUTC(created.Add(4 * time.Minute)),
		"model_sha256": strings.Repeat("a", 64), "prompt_sha256": strings.Repeat("b", 64), "config_sha256": strings.Repeat("c", 64),
		"payload": []byte(historyPayload), "payload_sha256": historyPayloadSHA,
		"response": []byte(historyResponse), "response_sha256": historyResponseSHA,
		"source_ids": []string{"behavior:HISTORY:1"}, "suggestions_enabled": true, "grouping_enabled": false,
		"input_tokens": 28, "output_tokens": 39,
		"reviews": []map[string]string{{
			"suggestion_id": "S-1", "actor": "reviewer", "at": board.FormatUTC(created.Add(3 * time.Minute)),
			"disposition": "reject", "edit": "",
		}},
	}
	if alter != nil {
		alter(history)
	}
	snapshot, err := bolt.Open(filepath.Clean(path), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	update := snapshot.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte("meta")).Put([]byte("ai_snapshot"), []byte("reb-ai-state/1")); err != nil {
			return err
		}
		for _, fixture := range []struct {
			bucket string
			data   any
		}{
			{"ai_history_v1", history},
			{"ai_budget_v1", map[string]any{"requested_at": requested, "state": "unresolved", "amount_microusd": 5000000}},
		} {
			data, err := tx.CreateBucket([]byte(fixture.bucket))
			if err != nil {
				return err
			}
			bindings, err := tx.CreateBucket([]byte(fixture.bucket + "_sha256"))
			if err != nil {
				return err
			}
			raw, err := json.Marshal(fixture.data)
			if err != nil {
				return err
			}
			id := []byte("REQ-SYNTHETIC-1")
			if err := data.Put(id, raw); err != nil {
				return err
			}
			if err := bindings.Put(id, []byte(fixtureHash(raw))); err != nil {
				return err
			}
		}
		return nil
	})
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if update != nil {
		t.Fatal(update)
	}
	return candidate.ID
}

func snapshotProtectedBytes(t *testing.T, path string) (payload, reply []byte) {
	t.Helper()
	snapshot, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	check := snapshot.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte("ai_history_v1")).Get([]byte("REQ-SYNTHETIC-1"))
		if raw == nil {
			return nil
		}
		var fixture struct{ Payload, Response []byte }
		if err := json.Unmarshal(raw, &fixture); err != nil {
			return err
		}
		payload, reply = fixture.Payload, fixture.Response
		return nil
	})
	if err := errors.Join(check, snapshot.Close()); err != nil {
		t.Fatal(err)
	}
	return payload, reply
}

func TestAIOffRestoredHistoryAndBudgetOnOperatorHealth(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	candidate := seedHistoricalAI(t, source, now, nil)
	settings := board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return now }}
	app, err := board.Open(source, settings)
	if err != nil {
		t.Fatal(err)
	}
	backup, backupError := app.Backup("reviewer", "pilot", filepath.Join(dir, "backups"))
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if backupError != nil {
		t.Fatal(backupError)
	}
	destination := filepath.Join(dir, "restored.db")
	if restored, err := board.RestoreBackup(backup.Path, destination, settings); err != nil || !restored.IntegrityOK {
		t.Fatalf("restore real synthetic snapshot: %+v %v", restored, err)
	}
	for _, snapshot := range []string{source, backup.Path, destination} {
		payload, reply := snapshotProtectedBytes(t, snapshot)
		if fixtureHash(payload) != historyPayloadSHA || fixtureHash(reply) != historyResponseSHA ||
			!bytes.Equal(payload, []byte(historyPayload)) || !bytes.Equal(reply, []byte(historyResponse)) {
			t.Fatal("whole-store custody changed the protected synthetic payload or response")
		}
	}
	app, err = board.Open(destination, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := app.Revalidate("reviewer", "pilot"); err != nil {
		t.Fatal(err)
	}
	handler := browser.New(app, false)
	t.Cleanup(handler.Close)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	operator, csrf := loginSession(t, server.URL, "reviewer")
	raw, status := response(t, operator.Get, server.URL+"/p/pilot/health")
	for _, want := range []string{"AI off", "Retained AI reviews", "REQ-SYNTHETIC-1", candidate, "reject", "5000000", "Budget exhausted", historyPayloadSHA, historyResponseSHA} {
		if status != http.StatusOK || !strings.Contains(raw, want) {
			t.Fatalf("restored AI-off Operator health did not show %q: HTTP %d", want, status)
		}
	}
	for _, secret := range []string{"PRIVATE-SYNTHETIC-REQUEST", "PRIVATE-SYNTHETIC-RESPONSE"} {
		if strings.Contains(raw, secret) {
			t.Fatal("operator health disclosed private historical text")
		}
	}
	viewer, _ := loginSession(t, server.URL, "viewer")
	raw, status = response(t, viewer.Get, server.URL+"/p/pilot/health")
	if status != http.StatusForbidden || strings.Contains(raw, "REQ-SYNTHETIC-1") {
		t.Fatalf("Viewer received protected historical summary: HTTP %d", status)
	}

	assertExhausted := func(t *testing.T) {
		t.Helper()
		health, err := app.Health("reviewer", "pilot")
		if err != nil || health.HistoricalAI == nil || !health.HistoricalAI.Exhausted || health.HistoricalAI.Remaining != 0 ||
			health.HistoricalAI.Held != 5000000 || len(health.HistoricalAI.Requests) != 1 {
			t.Fatalf("core action precondition did not retain the actual exhausted budget: %+v, %v", health.HistoricalAI, err)
		}
	}
	fixture := authorityFixture{visit: server.URL}
	reviewer := authoritySession{actor: "reviewer", client: operator, csrf: csrf}
	candidatePath := "/p/pilot/candidates/" + candidate
	t.Run("import", func(t *testing.T) {
		assertExhausted(t)
		pair, _, _, err := app.Candidate("reviewer", "pilot", candidate)
		if err != nil {
			t.Fatal(err)
		}
		stamp := board.FormatUTC(now)
		csv := followupBrowserCSV(t, followupBrowserResult(pair, "HISTORY", "HISTORY", "P-HISTORY", "T-HISTORY", "RUN-BUDGET", "RES-BUDGET", stamp, "PASS"))
		staged := fixture.send(t, reviewer, candidatePath+"/preview", nil, csv)
		token := authorityPreviewToken.FindStringSubmatch(staged.body)
		if staged.status != http.StatusOK || len(token) != 2 || !strings.Contains(staged.body, "RUN-BUDGET") {
			t.Fatalf("budget-exhausted reviewer preview did not return the current run: HTTP %d", staged.status)
		}
		committed := fixture.send(t, reviewer, candidatePath+"/commit", url.Values{"preview": {token[1]}}, nil)
		id, usable := strings.CutPrefix(committed.location, "/p/pilot/receipts/")
		if committed.status != http.StatusSeeOther || !usable || id == "" {
			t.Fatalf("budget-exhausted core commit did not produce a new receipt: HTTP %d location=%q", committed.status, committed.location)
		}
		receipt, err := app.Receipt("reviewer", "pilot", id)
		original, downloaded := response(t, operator.Get, server.URL+committed.location+"/source")
		if err != nil || receipt.LedgerHighWater != 2 || receipt.Hash != fixtureHash(csv) || downloaded != http.StatusOK || !bytes.Equal([]byte(original), csv) {
			t.Fatalf("core import failed exact original custody while exhausted: ledger=%d status=%d error=%v", receipt.LedgerHighWater, downloaded, err)
		}
		assertExhausted(t)
		recordThresholdHTTPInstance(t, "AC-020-06.P001", map[string]any{"coreAction": "import"}, "Core remains available")
	})
	t.Run("assessment_status", func(t *testing.T) {
		assertExhausted(t)
		body, returned := response(t, operator.Get, server.URL+candidatePath)
		if returned != http.StatusOK || !strings.Contains(body, `class="status SUFFICIENT"`) || !strings.Contains(body, "PASS 1") ||
			!strings.Contains(body, candidate) || !strings.Contains(body, "RUN-BUDGET") {
			t.Fatalf("budget-exhausted candidate did not show its actual current decision: HTTP %d", returned)
		}
		recordThresholdHTTPInstance(t, "AC-020-06.P002", map[string]any{"coreAction": "assessment status"}, "Core remains available")
	})
	t.Run("report_export", func(t *testing.T) {
		assertExhausted(t)
		frozen := fixture.send(t, reviewer, candidatePath+"/reports", nil, nil)
		id, usable := strings.CutPrefix(frozen.location, "/p/pilot/reports/")
		if frozen.status != http.StatusSeeOther || !usable || id == "" {
			t.Fatalf("budget-exhausted report freeze did not return a report: HTTP %d location=%q", frozen.status, frozen.location)
		}
		metadata, retained, err := app.ReportHTML("reviewer", "pilot", id)
		downloaded, returned := response(t, viewer.Get, server.URL+frozen.location+"?download=1")
		if err != nil || metadata.CandidateID != candidate || metadata.Status != board.Sufficient || metadata.LedgerHighWater != 2 ||
			returned != http.StatusOK || len(retained) == 0 || !bytes.Equal([]byte(downloaded), retained) || fixtureHash(retained) != metadata.SHA256 {
			t.Fatalf("budget-exhausted export did not preserve the frozen decision bytes: HTTP %d error=%v", returned, err)
		}
		assertExhausted(t)
		recordThresholdHTTPInstance(t, "AC-020-06.P003", map[string]any{"coreAction": "report export"}, "Core remains available")
	})
}

func TestAIOffRejectsMalformedRestoredState(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for _, fault := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"payload-hash", func(row map[string]any) { row["payload_sha256"] = strings.Repeat("9", 64) }},
		{"wrong-citation", func(row map[string]any) { row["source_ids"] = []string{"behavior:HISTORY:2"} }},
		{"response-shape", func(row map[string]any) {
			bad := []byte(`{"groups":[],"suggestions":[],"groups":[]}`)
			row["response"], row["response_sha256"] = bad, fixtureHash(bad)
		}},
		{"disposition", func(row map[string]any) {
			row["reviews"].([]map[string]string)[0]["suggestion_id"] = "other-suggestion"
		}},
		{"future-disabled", func(row map[string]any) { row["disabled_at"] = board.FormatUTC(now.Add(time.Hour)) }},
	} {
		t.Run(fault.name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.db")
			candidate := seedHistoricalAI(t, source, now, fault.change)
			raw, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(source+".sha256", []byte(fixtureHash(raw)+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			settings := board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return now }}
			path := filepath.Join(dir, "restored.db")
			result, err := board.RestoreBackup(source, path, settings)
			if err != nil || result.IntegrityOK {
				t.Fatalf("malformed but correctly checksummed archive did not enter quarantine: %+v %v", result, err)
			}
			app, err := board.Open(path, settings)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := app.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err := app.Assess("reviewer", "pilot", candidate); !errors.Is(err, board.ErrIntegrity) {
				t.Fatalf("invalid archival state allowed fresh assessment of otherwise sufficient candidate: %v", err)
			}
			if _, err := app.Freeze("reviewer", "pilot", candidate); !errors.Is(err, board.ErrIntegrity) {
				t.Fatalf("invalid archival state allowed fresh report: %v", err)
			}
			if err := app.Revalidate("reviewer", "pilot"); !errors.Is(err, board.ErrIntegrity) {
				t.Fatalf("revalidation cleared quarantine: %v", err)
			}
		})
	}
}

func TestAIOffPurgeRemovesPrivateHistoryAndRetainsUnresolvedFacts(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "history.db")
	candidate := seedHistoricalAI(t, path, now, nil)
	beforePayload, beforeReply := snapshotProtectedBytes(t, path)
	if !bytes.Equal(beforePayload, []byte(historyPayload)) || !bytes.Equal(beforeReply, []byte(historyResponse)) ||
		fixtureHash(beforePayload) != historyPayloadSHA || fixtureHash(beforeReply) != historyResponseSHA {
		t.Fatal("candidate-linked payload and response were not both retained before expiry")
	}
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
	ids := []string{candidate}
	plan, err := app.PreviewPurge("reviewer", "pilot", ids)
	if err != nil || len(plan.AIRequestIDs) != 1 || plan.AIRequestIDs[0] != "REQ-SYNTHETIC-1" {
		t.Fatalf("purge did not preview candidate-linked history: %+v, %v", plan, err)
	}
	if _, err := app.Purge("reviewer", "pilot", ids, plan.Confirmation); err != nil {
		t.Fatal(err)
	}
	health, err := app.Health("reviewer", "pilot")
	if err != nil || health.HistoricalAI == nil || len(health.HistoricalAI.Requests) != 0 ||
		health.HistoricalAI.RequestsToday != 0 || health.HistoricalAI.Held != 5000000 ||
		health.HistoricalAI.Remaining != 0 || !health.HistoricalAI.Exhausted {
		t.Fatalf("purged history or carried UTC holds are wrong: %+v, %v", health.HistoricalAI, err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	payload, reply := snapshotProtectedBytes(t, path)
	if payload != nil || reply != nil {
		t.Fatal("confirmed purge left reachable private bytes")
	}
	t.Log(`REB_ACCEPTANCE_INSTANCE {"id":"AC-014-10.P001","case_id":"AC-014-10","parameters":{},"actual":"Protected payload and response follow candidate expiry"}`)
}

func TestAIHistoryFixtureProcess(t *testing.T) {
	if path := os.Getenv("REB_TEST_AI_SNAPSHOT_PATH"); path != "" {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("synthetic whole-store target must be absent: %v", err)
		}
		seedHistoricalAI(t, path, time.Now().UTC(), nil)
	}
	if path := os.Getenv("REB_TEST_COMPARE_AI_SNAPSHOT_PATH"); path != "" {
		payload, reply := snapshotProtectedBytes(t, path)
		if fixtureHash(payload) != historyPayloadSHA || fixtureHash(reply) != historyResponseSHA {
			t.Fatal("synthetic snapshot payload or response changed")
		}
	}
}
