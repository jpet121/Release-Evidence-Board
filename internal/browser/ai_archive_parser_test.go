package browser_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"release-evidence-board/internal/board"
)

const fixtureRequestID = "REQ-SYNTHETIC-1"

type archiveFault struct {
	name   string
	change func(*bolt.Tx) error
}

func editArchiveRecord(bucket string, patch func(map[string]any)) func(*bolt.Tx) error {
	return func(tx *bolt.Tx) error {
		var item map[string]any
		if err := json.Unmarshal(tx.Bucket([]byte(bucket)).Get([]byte(fixtureRequestID)), &item); err != nil {
			return err
		}
		patch(item)
		return writeArchiveFixtureRecord(tx, bucket, fixtureRequestID, item)
	}
}

func writeArchiveFixtureRecord(tx *bolt.Tx, bucket, key string, item any) error {
	raw, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if err := tx.Bucket([]byte(bucket)).Put([]byte(key), raw); err != nil {
		return err
	}
	return tx.Bucket([]byte(bucket+"_sha256")).Put([]byte(key), []byte(fixtureHash(raw)))
}

func archiveReply(raw string, grouping bool) func(*bolt.Tx) error {
	return editArchiveRecord("ai_history_v1", func(item map[string]any) {
		item["response"], item["response_sha256"], item["grouping_enabled"] = []byte(raw), fixtureHash([]byte(raw)), grouping
	})
}

func archiveReplyWithoutReviews(raw string) func(*bolt.Tx) error {
	return editArchiveRecord("ai_history_v1", func(item map[string]any) {
		item["response"], item["response_sha256"] = []byte(raw), fixtureHash([]byte(raw))
		item["reviews"] = []map[string]string{}
	})
}

func mutateArchiveFixture(t *testing.T, path string, original []byte, update func(*bolt.Tx) error) {
	t.Helper()
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	change := db.Update(update)
	if err := errors.Join(change, db.Close()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".sha256", []byte(fixtureHash(raw)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAIOffArchiveRestoreRejectsOneInvalidContractField(t *testing.T) {
	now := interruptionFixtureTime
	dir := t.TempDir()
	path := filepath.Join(dir, "base.db")
	candidate := seedHistoricalAI(t, path, now, nil)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return now }}

	historyField := func(name, key string, value any) archiveFault {
		return archiveFault{name, editArchiveRecord("ai_history_v1", func(item map[string]any) { item[key] = value })}
	}
	budgetField := func(name, key string, value any) archiveFault {
		return archiveFault{name, editArchiveRecord("ai_budget_v1", func(item map[string]any) { item[key] = value })}
	}
	suggestion := strings.TrimPrefix(strings.TrimSuffix(historyResponse, `],"groups":[]}`), `{"suggestions":[`)
	faults := []archiveFault{
		{"invalid-json-first-token", archiveReply(`!`, false)},
		{"invalid-json-key", archiveReply(`{"suggestions":[],1:[]}`, false)},
		{"invalid-json-value", archiveReply(`{"suggestions":!}`, false)},
		{"missing-json-colon", archiveReply(`{"suggestions" []}`, false)},
		{"missing-json-close", archiveReply(strings.TrimSuffix(historyResponse, "}"), false)},
		{"trailing-json-document", archiveReply(historyResponse+`false`, false)},
		{"trailing-json-comma", archiveReply(strings.TrimSuffix(historyResponse, "}")+`,}`, false)},
		{"missing-suggestion-array", archiveReplyWithoutReviews(`{"groups":[]}`)},
		{"missing-group-array", archiveReplyWithoutReviews(`{"suggestions":[]}`)},
		{"object-instead-of-suggestion-array", archiveReplyWithoutReviews(`{"suggestions":{},"groups":[]}`)},
		{"text-instead-of-group-array", archiveReplyWithoutReviews(`{"suggestions":[],"groups":"none"}`)},
		{"missing-review-array", editArchiveRecord("ai_history_v1", func(item map[string]any) { delete(item, "reviews") })},
		{"object-instead-of-review-array", editArchiveRecord("ai_history_v1", func(item map[string]any) { item["reviews"] = map[string]string{} })},
		{"future-schema", func(tx *bolt.Tx) error {
			return tx.Bucket([]byte("meta")).Put([]byte("ai_snapshot"), []byte("reb-ai-state/2"))
		}},
		{"missing-schema", func(tx *bolt.Tx) error { return tx.Bucket([]byte("meta")).Delete([]byte("ai_snapshot")) }},
		{"schema-key-is-bucket", func(tx *bolt.Tx) error {
			meta := tx.Bucket([]byte("meta"))
			if err := meta.Delete([]byte("ai_snapshot")); err != nil {
				return err
			}
			_, err := meta.CreateBucket([]byte("ai_snapshot"))
			return err
		}},
		{"missing-bound-section", func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte("ai_history_v1_sha256")) }},
		{"orphan-bound-key", func(tx *bolt.Tx) error {
			return tx.Bucket([]byte("ai_history_v1_sha256")).Put([]byte("UNMATCHED"), []byte(historyPayloadSHA))
		}},
		{"record-binding-mismatch", func(tx *bolt.Tx) error {
			return tx.Bucket([]byte("ai_history_v1_sha256")).Put([]byte(fixtureRequestID), []byte(historyPayloadSHA))
		}},
		{"invalid-archive-key", func(tx *bolt.Tx) error {
			return writeArchiveFixtureRecord(tx, "ai_budget_v1", "invalid/key", map[string]any{
				"requested_at": board.FormatUTC(now), "state": "billed", "amount_microusd": 0,
			})
		}},
		{"unexpected-nested-history-record", func(tx *bolt.Tx) error {
			_, err := tx.Bucket([]byte("ai_history_v1")).CreateBucket([]byte("NOT-A-VALUE"))
			return err
		}},
		{"oversize-bound-record", func(tx *bolt.Tx) error {
			key := []byte(fixtureRequestID)
			bucket := tx.Bucket([]byte("ai_history_v1"))
			raw := append(append([]byte(nil), bucket.Get(key)...), strings.Repeat(" ", 16<<20)...)
			if err := bucket.Put(key, raw); err != nil {
				return err
			}
			return tx.Bucket([]byte("ai_history_v1_sha256")).Put(key, []byte(fixtureHash(raw)))
		}},
		budgetField("unrecognized-accounting-state", "state", "estimated"),
		budgetField("negative-accounting", "amount_microusd", -1),
		budgetField("zero-unresolved-hold", "amount_microusd", 0),
		budgetField("amount-is-text", "amount_microusd", "5000000"),
		budgetField("not-utc-accounting", "requested_at", "2026-11-19T12:00:00+00:00"),
		budgetField("future-accounting", "requested_at", board.FormatUTC(now.Add(time.Hour))),
		{"overflow-held-total", func(tx *bolt.Tx) error {
			return writeArchiveFixtureRecord(tx, "ai_budget_v1", "ADDITIONAL-HOLD", map[string]any{
				"requested_at": board.FormatUTC(now.Add(-time.Hour)), "state": "unresolved", "amount_microusd": int64(math.MaxInt64),
			})
		}},
		historyField("unbound-request-time", "requested_at", board.FormatUTC(now.Add(-3*time.Minute))),
		historyField("invalid-request-time", "requested_at", "not UTC"),
		historyField("invalid-completion-time", "completed_at", "not UTC"),
		historyField("invalid-disable-time", "disabled_at", "not UTC"),
		historyField("completed-before-request", "completed_at", board.FormatUTC(now.Add(-6*time.Minute))),
		historyField("disabled-before-completion", "disabled_at", board.FormatUTC(now.Add(-4*time.Minute))),
		{"request-before-claimed-candidate-existed", func(tx *bolt.Tx) error {
			stamp := board.FormatUTC(now.Add(-6 * time.Minute))
			if err := editArchiveRecord("ai_budget_v1", func(item map[string]any) { item["requested_at"] = stamp })(tx); err != nil {
				return err
			}
			return editArchiveRecord("ai_history_v1", func(item map[string]any) { item["requested_at"] = stamp })(tx)
		}},
		historyField("missing-candidate", "candidate_id", "C-DOES-NOT-EXIST"),
		historyField("malformed-requester", "requester", "unavailable/actor"),
		historyField("model-hash", "model_sha256", "an-unbound-model"),
		historyField("prompt-hash", "prompt_sha256", "an-unbound-prompt"),
		historyField("config-hash", "config_sha256", "an-unbound-config"),
		historyField("missing-input-token-count", "input_tokens", 0),
		historyField("too-many-input-tokens", "input_tokens", 20_001),
		historyField("missing-output-token-count", "output_tokens", 0),
		historyField("too-many-output-tokens", "output_tokens", 4_001),
		historyField("response-hash-mismatch", "response_sha256", historyPayloadSHA),
		historyField("no-historical-capability", "suggestions_enabled", false),
		historyField("null-review-array", "reviews", nil),
		historyField("empty-source-array", "source_ids", []string{}),
		historyField("duplicate-source", "source_ids", []string{"behavior:HISTORY:1", "behavior:HISTORY:1"}),
		historyField("unretained-failure", "source_ids", []string{"result:DOES-NOT-EXIST"}),
		historyField("pass-is-not-requested-failure", "source_ids", []string{"result:RES-HISTORY"}),
		historyField("unpaired-behavior-revision", "source_ids", []string{"behavior:HISTORY:2"}),
		historyField("missing-behavior-revision", "source_ids", []string{"behavior:HISTORY"}),
		historyField("unsupported-source-kind", "source_ids", []string{"file:HISTORY:1"}),
		{"empty-bound-payload", editArchiveRecord("ai_history_v1", func(item map[string]any) {
			item["payload"], item["payload_sha256"] = []byte{}, fixtureHash(nil)
		})},
		{"invalid-protected-utf8", editArchiveRecord("ai_history_v1", func(item map[string]any) {
			raw := []byte{0xff}
			item["payload"], item["payload_sha256"] = raw, fixtureHash(raw)
		})},
		{"response-is-array", archiveReply(`[]`, false)},
		{"invalid-response-utf8", archiveReply(strings.Replace(historyResponse, "PRIVATE-SYNTHETIC-RESPONSE", "\xff", 1), false)},
		{"trailing-json", archiveReply(historyResponse+" {}", false)},
		{"null-suggestion-array", archiveReplyWithoutReviews(`{"suggestions":null,"groups":[]}`)},
		{"null-group-array", archiveReplyWithoutReviews(`{"suggestions":[],"groups":null}`)},
		{"too-many-suggestions", archiveReply(`{"suggestions":[`+strings.Repeat(suggestion+",", 20)+suggestion+`],"groups":[]}`, false)},
		{"too-many-groups", archiveReply(`{"suggestions":[],"groups":[`+strings.Repeat("{},", 20)+`{}]}`, true)},
		{"mis-cased-json-key", archiveReply(strings.Replace(historyResponse, "\"groups\"", "\"Groups\"", 1), false)},
		{"repeated-json-key", archiveReply(strings.Replace(historyResponse, "\"groups\":[]", "\"groups\":[],\"groups\":[]", 1), false)},
		{"unknown-json-key", archiveReply(strings.Replace(historyResponse, "\"groups\":[]", "\"groups\":[],\"comment\":\"\"", 1), false)},
		{"explicit-null-field", archiveReply(strings.Replace(historyResponse, "\"question\":\"\"", "\"question\":null", 1), false)},
		{"lone-high-surrogate", archiveReply(strings.Replace(historyResponse, "PRIVATE-SYNTHETIC-RESPONSE", `\uD800`, 1), false)},
		{"high-surrogate-at-end-of-object", archiveReply(`{"groups":[],"suggestions":[`+
			strings.Replace(suggestion, `"question":""`, `"question":"\uD800"`, 1)+`]}`, false)},
		{"lone-low-surrogate", archiveReply(strings.Replace(historyResponse, "PRIVATE-SYNTHETIC-RESPONSE", `\uDC00`, 1), false)},
		{"high-surrogate-followed-by-text", archiveReply(strings.Replace(historyResponse, "PRIVATE-SYNTHETIC-RESPONSE", `\uD800separate`, 1), false)},
		{"high-surrogate-followed-by-newline", archiveReply(strings.Replace(historyResponse, "PRIVATE-SYNTHETIC-RESPONSE", `\uD800\nneutral`, 1), false)},
		{"second-surrogate-is-high", archiveReply(strings.Replace(historyResponse, "PRIVATE-SYNTHETIC-RESPONSE", `\uD800\uD801`, 1), false)},
		{"second-scalar-is-above-surrogates", archiveReply(strings.Replace(historyResponse, "PRIVATE-SYNTHETIC-RESPONSE", `\uD800\uE000`, 1), false)},
		{"non-low-surrogate-pair", archiveReply(strings.Replace(historyResponse, "PRIVATE-SYNTHETIC-RESPONSE", `\uD800\u1234`, 1), false)},
		{"suggestion-is-array", archiveReply(`{"suggestions":[[]],"groups":[]}`, false)},
		{"invalid-suggestion-id", archiveReply(strings.Replace(historyResponse, `"S-1"`, `"S/1"`, 1), false)},
		{"duplicate-suggestion-id", archiveReply(`{"suggestions":[`+suggestion+","+suggestion+`],"groups":[]}`, false)},
		{"unknown-suggestion-behavior", archiveReply(strings.Replace(historyResponse, `"behavior_id":"HISTORY"`, `"behavior_id":"ABSENT"`, 1), false)},
		{"unattributed-suggestion-citation", archiveReply(strings.Replace(historyResponse, "behavior:HISTORY:1", "behavior:HISTORY:2", 1), false)},
		{"needs-review-without-question", archiveReply(strings.Replace(historyResponse, "\"clear\"", "\"needs_review\"", 1), false)},
		{"oversize-proposed-test", archiveReply(strings.Replace(historyResponse, "PRIVATE-SYNTHETIC-RESPONSE", strings.Repeat("é", 2001), 1), false)},
		{"missing-suggestion-rationale", archiveReply(strings.Replace(historyResponse, "Candidate behavior example", "", 1), false)},
		{"oversize-suggestion-question", archiveReply(strings.Replace(historyResponse, `"question":""`, `"question":"`+strings.Repeat("é", 2001)+`"`, 1), false)},
		{"invalid-suggestion-uncertainty", archiveReply(strings.Replace(historyResponse, `"clear"`, `"unsupported"`, 1), false)},
		{"suggestion-without-citation", archiveReply(strings.Replace(historyResponse, `["behavior:HISTORY:1"]`, `[]`, 1), false)},
		{"forbidden-group-array", archiveReply(`{"suggestions":[],"groups":[{"id":"G1","label":"protected","source_row_ids":["X","Y"]}]}`, false)},
		{"unattributed-group", archiveReply(`{"suggestions":[],"groups":[{"id":"G1","label":"protected","source_row_ids":["X","Y"]}]}`, true)},
		{"group-is-array", archiveReply(`{"suggestions":[],"groups":[[]]}`, true)},
		{"invalid-group-id", archiveReply(`{"suggestions":[],"groups":[{"id":"G/1","label":"protected","source_row_ids":["X","Y"]}]}`, true)},
		{"empty-group-label", archiveReply(`{"suggestions":[],"groups":[{"id":"G1","label":"","source_row_ids":["X","Y"]}]}`, true)},
		{"forbidden-suggestion-array", editArchiveRecord("ai_history_v1", func(item map[string]any) {
			item["suggestions_enabled"], item["grouping_enabled"] = false, true
		})},
		historyField("unordered-review-events", "reviews", []map[string]string{
			{"suggestion_id": "S-1", "actor": "archived-reviewer", "at": board.FormatUTC(now.Add(-time.Minute)), "disposition": "reject", "edit": ""},
			{"suggestion_id": "S-1", "actor": "archived-reviewer", "at": board.FormatUTC(now.Add(-2 * time.Minute)), "disposition": "reject", "edit": ""},
		}),
		historyField("empty-edit", "reviews", []map[string]string{
			{"suggestion_id": "S-1", "actor": "archived-reviewer", "at": board.FormatUTC(now.Add(-2 * time.Minute)), "disposition": "edit", "edit": ""},
		}),
		historyField("incomplete-review", "reviews", []map[string]string{
			{"suggestion_id": "S-1", "actor": "archived-reviewer", "at": board.FormatUTC(now.Add(-2 * time.Minute)), "disposition": "reject"},
		}),
		historyField("invalid-review-time", "reviews", []map[string]string{
			{"suggestion_id": "S-1", "actor": "archived-reviewer", "at": "not UTC", "disposition": "reject", "edit": ""},
		}),
		historyField("review-after-disable", "reviews", []map[string]string{
			{"suggestion_id": "S-1", "actor": "archived-reviewer", "at": board.FormatUTC(now), "disposition": "reject", "edit": ""},
		}),
		historyField("invalid-review-actor", "reviews", []map[string]string{
			{"suggestion_id": "S-1", "actor": "invalid/reviewer", "at": board.FormatUTC(now.Add(-2 * time.Minute)), "disposition": "reject", "edit": ""},
		}),
		historyField("reject-with-edit", "reviews", []map[string]string{
			{"suggestion_id": "S-1", "actor": "archived-reviewer", "at": board.FormatUTC(now.Add(-2 * time.Minute)), "disposition": "reject", "edit": "unexpected"},
		}),
	}
	for _, fault := range faults {
		t.Run(fault.name, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "corrupt.db")
			mutateArchiveFixture(t, source, original, fault.change)
			destination := filepath.Join(t.TempDir(), "restored.db")
			result, err := board.RestoreBackup(source, destination, cfg)
			if err != nil || result.IntegrityOK {
				t.Fatalf("transport checksum concealed an invalid archive premise: %+v, %v", result, err)
			}
			app, err := board.Open(destination, cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, reportErr := app.Freeze("reviewer", "pilot", candidate)
			_, assessmentErr := app.Assess("reviewer", "pilot", candidate)
			if err := app.Close(); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(reportErr, board.ErrIntegrity) || !errors.Is(assessmentErr, board.ErrIntegrity) {
				t.Fatalf("damaged archive allowed a fresh report or decision: %v, %v", reportErr, assessmentErr)
			}
		})
	}
}

func TestAIOffArchiveAcceptsPresentEmptyArrays(t *testing.T) {
	now := interruptionFixtureTime
	path := filepath.Join(t.TempDir(), "empty-arrays.db")
	candidate := seedHistoricalAI(t, path, now, func(item map[string]any) {
		raw := []byte(`{"suggestions":[],"groups":[]}`)
		item["response"], item["response_sha256"] = raw, fixtureHash(raw)
		item["reviews"] = []map[string]string{}
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
	health, err := app.Health("reviewer", "pilot")
	if err != nil || health.HistoricalAI == nil || len(health.HistoricalAI.Requests) != 1 ||
		len(health.HistoricalAI.Requests[0].Reviews) != 0 {
		t.Fatalf("present empty response/review arrays rejected: %+v, %v", health.HistoricalAI, err)
	}
	if _, err := app.Assess("reviewer", "pilot", candidate); err != nil {
		t.Fatalf("valid empty arrays incorrectly quarantined assessment: %v", err)
	}
	if _, err := app.Freeze("reviewer", "pilot", candidate); err != nil {
		t.Fatalf("valid empty arrays incorrectly blocked frozen report: %v", err)
	}
}

func TestAIOffArchiveUTCAccountingAndRemovedReviewer(t *testing.T) {
	now := interruptionFixtureTime
	dir := t.TempDir()
	path := filepath.Join(dir, "base.db")
	seedHistoricalAI(t, path, now, nil)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		count, quota int
		exhausted    bool
		disposition  string
		edit         string
	}{{0, 2, false, "edit", "PRIVATE-ARCHIVED-EDIT"}, {18, 20, true, "accept", ""}} {
		t.Run(fmt.Sprint(tc.quota, "-requests"), func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "accounted.db")
			mutateArchiveFixture(t, src, original, func(tx *bolt.Tx) error {
				if err := editArchiveRecord("ai_history_v1", func(item map[string]any) {
					item["requester"] = "former-requester"
					item["reviews"] = []map[string]string{{"suggestion_id": "S-1", "actor": "former-reviewer",
						"at": board.FormatUTC(now.Add(-2 * time.Minute)), "disposition": tc.disposition, "edit": tc.edit}}
					reply := strings.Replace(historyResponse, "PRIVATE-SYNTHETIC-RESPONSE", `\uD83D\uDE80\u0061\uE000\n\"`, 1)
					reply = strings.Replace(reply, `"uncertainty":"clear","question":""`, `"uncertainty":"needs_review","question":"Which recorded fixture applies?"`, 1)
					item["response"], item["response_sha256"] = []byte(reply), fixtureHash([]byte(reply))
				})(tx); err != nil {
					return err
				}
				if err := editArchiveRecord("ai_budget_v1", func(item map[string]any) { item["amount_microusd"] = 2_500_000 })(tx); err != nil {
					return err
				}
				facts := []struct {
					id, state string
					at        time.Time
					amount    int
				}{{"YESTERDAY-HOLD", "unresolved", now.Add(-24 * time.Hour), 1_125_000},
					{"YESTERDAY-BILL", "billed", now.Add(-24 * time.Hour), 1_000_000},
					{"TODAY-BILL", "billed", now, 400_000}}
				for i := 0; i < tc.count; i++ {
					facts = append(facts, struct {
						id, state string
						at        time.Time
						amount    int
					}{fmt.Sprintf("EXTRA-%02d", i), "billed", now, 0})
				}
				for _, fact := range facts {
					if err := writeArchiveFixtureRecord(tx, "ai_budget_v1", fact.id, map[string]any{
						"requested_at": board.FormatUTC(fact.at), "state": fact.state, "amount_microusd": fact.amount,
					}); err != nil {
						return err
					}
				}
				return nil
			})
			config := board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return now }}
			dst := filepath.Join(t.TempDir(), "restored.db")
			restored, err := board.RestoreBackup(src, dst, config)
			if err != nil || !restored.IntegrityOK {
				t.Fatalf("valid historical identity no longer assigned or exact accounting rejected: %+v, %v", restored, err)
			}
			app, err := board.Open(dst, config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := app.Close(); err != nil {
					t.Error(err)
				}
			})
			health, err := app.Health("reviewer", "pilot")
			state := health.HistoricalAI
			if err != nil || state == nil || state.RequestsToday != tc.quota || state.Held != 3_625_000 ||
				state.BilledToday != 400_000 || state.Remaining != 975_000 || state.Exhausted != tc.exhausted {
				t.Fatalf("UTC billing, outstanding holds, or count cap differs: %+v, %v", state, err)
			}
			if len(state.Requests) != 1 || len(state.Requests[0].Reviews) != 1 ||
				state.Requests[0].Reviews[0].Actor != "former-reviewer" || state.Requests[0].Reviews[0].Disposition != tc.disposition {
				t.Fatal("detached historical reviewer changed on restore")
			}
		})
	}
}
