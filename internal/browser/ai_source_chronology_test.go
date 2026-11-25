package browser_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"release-evidence-board/internal/board"
)

func TestAIOffRestoreRejectsFailureUnavailableAtRequest(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		added  time.Duration
		closed time.Duration
		group  string
		other  bool
		valid  bool
	}{{"before-request", -270 * time.Second, -280 * time.Second, "", false, true},
		{"imported-after-request", -150 * time.Second, -280 * time.Second, "", false, false},
		{"producer-close-after-request", -270 * time.Second, -220 * time.Second, "", false, false},
		{"same-time-failure-belongs-to-independent-candidate", -270 * time.Second, -280 * time.Second, "", true, false},
		{"valid-group-of-selected-failures", -270 * time.Second, -280 * time.Second, "valid", false, true},
		{"valid-groups-with-suggestions-disabled", -270 * time.Second, -280 * time.Second, "groups-only", false, true},
		{"group-repeats-one-failure", -270 * time.Second, -280 * time.Second, "repeated", false, false},
		{"group-cites-unselected-failure", -270 * time.Second, -280 * time.Second, "unselected", false, false},
		{"repeated-group-identifier", -270 * time.Second, -280 * time.Second, "duplicate-group", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.db")
			id := seedHistoricalAI(t, source, now, nil)
			// Open normally, then set the Board fixture clock for a real import
			// so the product records actual import custody and producer finish.
			clock := now
			// Use the otherwise valid snapshot without importing through its
			// historical state. Restore it into that past timeline afterward.
			store, err := bolt.Open(source, 0600, nil)
			if err != nil {
				t.Fatal(err)
			}
			buckets := []string{"ai_history_v1", "ai_history_v1_sha256", "ai_budget_v1", "ai_budget_v1_sha256"}
			entries := make(map[string][]byte, len(buckets))
			key := []byte("REQ-SYNTHETIC-1")
			change := store.Update(func(tx *bolt.Tx) error {
				for _, name := range buckets {
					entries[name] = append([]byte(nil), tx.Bucket([]byte(name)).Get(key)...)
					if err := tx.DeleteBucket([]byte(name)); err != nil {
						return err
					}
				}
				return tx.Bucket([]byte("meta")).Delete([]byte("ai_snapshot"))
			})
			if err := errors.Join(change, store.Close()); err != nil {
				t.Fatal(err)
			}
			settings := board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return clock }}
			app, err := board.Open(source, settings)
			if err != nil {
				t.Fatal(err)
			}
			candidate, manifest, _, err := app.Candidate("reviewer", "pilot", id)
			if err != nil {
				_ = app.Close()
				t.Fatal(err)
			}
			release := manifest.Manifest.ReleaseID
			if tc.other {
				clock = now.Add(-290 * time.Second)
				independent := manifest.Manifest
				independent.ReleaseID = "OTHER-HISTORY"
				independent.EnvironmentID = "OTHER-ENV"
				created, reused, err := app.CreateCandidate("reviewer", "pilot", board.CandidateInput{
					Manifest: independent, BaselineDigest: candidate.BaselineDigest,
				})
				if err != nil || reused || created.ID == id {
					_ = app.Close()
					t.Fatalf("failure did not acquire a separate valid candidate: %v", err)
				}
				candidate, release = created, independent.ReleaseID
			}
			clock = now.Add(tc.added)
			finish := board.FormatUTC(now.Add(tc.closed))
			raw := []byte(followupHeader + fmt.Sprintf("reb-results/1.3,RESULT,RES-CITED-FAIL,DEMO,%s,%s,%s,HISTORY,1,P-HISTORY,1,T-HISTORY,RUN-CITED-FAIL,1,%s,%s,FAIL,,\n",
				release, candidate.ManifestDigest, candidate.BaselineDigest, finish, finish))
			if tc.name == "before-request" {
				raw = append(raw, fmt.Sprintf("reb-results/1.3,RUN_ONLY,,DEMO,HISTORY,%s,%s,HISTORY,1,P-HISTORY,1,,RUN-CITED-EMPTY,,,%s,,,\n",
					candidate.ManifestDigest, candidate.BaselineDigest, finish)...)
			}
			if tc.group != "" {
				raw = append(raw, fmt.Sprintf("reb-results/1.3,RESULT,RES-CITED-SECOND,DEMO,HISTORY,%s,%s,HISTORY,1,P-HISTORY,1,T-HISTORY,RUN-CITED-SECOND,1,%s,%s,FAIL,,\n",
					candidate.ManifestDigest, candidate.BaselineDigest, finish, finish)...)
			}
			preview, err := app.PreviewImport("reviewer", "pilot", candidate.ID, raw)
			if err != nil {
				_ = app.Close()
				t.Fatal(err)
			}
			receipt, importErr := app.CommitImport("reviewer", "pilot", preview.Token, raw)
			if err := errors.Join(importErr, app.Close()); err != nil || !receipt.ImportedAt.Equal(clock) {
				t.Fatalf("fixture did not commit actual timed FAIL: %+v, %v", receipt, err)
			}
			var history map[string]any
			if err := json.Unmarshal(entries["ai_history_v1"], &history); err != nil {
				t.Fatal(err)
			}
			sources := []string{"behavior:HISTORY:1", "result:RES-CITED-FAIL"}
			if tc.group != "" {
				history["grouping_enabled"] = true
				second := "RES-CITED-SECOND"
				if tc.group == "repeated" {
					second = "RES-CITED-FAIL"
				}
				if tc.group != "unselected" {
					sources = append(sources, "result:RES-CITED-SECOND")
				}
				grouped := fmt.Sprintf(`{"id":"G-CONTROL","label":"Prior synthetic group","source_row_ids":["RES-CITED-FAIL","%s"]}`, second)
				if tc.group == "duplicate-group" {
					grouped += "," + grouped
				}
				reply := strings.Replace(historyResponse, `"groups":[]`, `"groups":[`+grouped+"]", 1)
				if tc.group == "groups-only" {
					history["suggestions_enabled"] = false
					history["reviews"] = []any{}
					reply = `{"suggestions":[],"groups":[` + grouped + "]}"
				}
				history["response"], history["response_sha256"] = []byte(reply), fixtureHash([]byte(reply))
			}
			history["source_ids"] = sources
			entries["ai_history_v1"], err = json.Marshal(history)
			if err != nil {
				t.Fatal(err)
			}
			entries["ai_history_v1_sha256"] = []byte(fixtureHash(entries["ai_history_v1"]))
			store, err = bolt.Open(source, 0600, nil)
			if err != nil {
				t.Fatal(err)
			}
			change = store.Update(func(tx *bolt.Tx) error {
				if err := tx.Bucket([]byte("meta")).Put([]byte("ai_snapshot"), []byte("reb-ai-state/1")); err != nil {
					return err
				}
				for _, name := range buckets {
					bucket, err := tx.CreateBucket([]byte(name))
					if err != nil {
						return err
					}
					if err := bucket.Put(key, entries[name]); err != nil {
						return err
					}
				}
				return nil
			})
			if err := errors.Join(change, store.Close()); err != nil {
				t.Fatal(err)
			}
			bytes, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(source+".sha256", []byte(fixtureHash(bytes)+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			clock = now
			restored, err := board.RestoreBackup(source, filepath.Join(dir, "restored.db"), settings)
			if err != nil || restored.IntegrityOK != tc.valid {
				t.Fatalf("trusted-checksum restore accepted history availability=%t, wanted %t: %v", restored.IntegrityOK, tc.valid, err)
			}
			if tc.name == "before-request" {
				for _, alternate := range []string{"MISSING-RUN", "RUN-HISTORY", "RUN-CITED-EMPTY"} {
					t.Run(alternate, func(t *testing.T) {
						path := filepath.Join(t.TempDir(), "bound.db")
						mutateArchiveFixture(t, path, bytes, func(tx *bolt.Tx) error {
							raw, err := json.Marshal(alternate)
							if err != nil {
								return err
							}
							return tx.Bucket([]byte("results")).Put([]byte("RES-CITED-FAIL"), raw)
						})
						dest := filepath.Join(t.TempDir(), "held.db")
						report, err := board.RestoreBackup(path, dest, settings)
						if err != nil || report.IntegrityOK {
							t.Fatalf("corrupt result index did not hold the restore: %+v, %v", report, err)
						}
						app, err := board.Open(dest, settings)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = app.Close() })
						health, err := app.Health("reviewer", "pilot")
						if !errors.Is(err, board.ErrIntegrity) || health.HistoricalAI != nil ||
							!strings.Contains(err.Error(), "historical AI request cites a result outside its selected failure evidence") {
							t.Fatalf("operator summary trusted an index to %s: %+v, %v", alternate, health.HistoricalAI, err)
						}
					})
				}
			}
		})
	}
}
