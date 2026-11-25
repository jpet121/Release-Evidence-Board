package browser_test

import (
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"release-evidence-board/internal/board"
	"release-evidence-board/internal/browser"
)

func TestTrustedRestoreRejectsInvalidOperatingRecordsAndBlocksImport(t *testing.T) {
	clock := interruptionFixtureTime
	dir := t.TempDir()
	valid := filepath.Join(dir, "original.db")
	id := seedHistoricalAI(t, valid, clock, nil)
	original, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	cfg := board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return clock }}
	healthy, err := board.Open(valid, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defaults, configErr := healthy.OperatingConfig("reviewer", "pilot")
	_, roleErr := healthy.OperatingConfig("viewer", "pilot")
	initial, rateErr := healthy.ImportRate("reviewer", "pilot")
	_, readRateRoleErr := healthy.ImportRate("viewer", "pilot")
	invalidRateErr := healthy.SetImportRate("reviewer", "pilot", board.ImportRatePolicy{Count: 2})
	if err := errors.Join(configErr, rateErr, healthy.Close()); err != nil ||
		!errors.Is(roleErr, board.ErrForbidden) || !errors.Is(readRateRoleErr, board.ErrForbidden) ||
		!errors.Is(invalidRateErr, board.ErrInvalid) || defaults["backup-interval"] != 24*time.Hour ||
		initial != (board.ImportRatePolicy{Count: 10, Window: time.Minute}) {
		t.Fatalf("unchanged store did not return supported defaults and deny read to Viewer: %v, %+v, %v, %v", defaults, initial, roleErr, err)
	}
	for _, record := range []struct {
		key     string
		valid   string
		invalid []string
	}{
		{"operating_config", `{"evidence-retention":1,"audit-retention":1,"backup-expiry":1,"backup-interval":1,"backup-overdue":1}`, []string{
			`[]`, `{"backup-interval":1}`,
			`{"evidence-retention":1,"audit-retention":1,"backup-expiry":1,"backup-interval":0,"backup-overdue":1}`,
		}},
		{"import_rate_policy", `{"count":2,"window":60000000000}`, []string{
			`[]`, `{"count":0,"window":60000000000}`, `{"count":2,"window":0}`,
		}},
	} {
		t.Run(record.key, func(t *testing.T) {
			key, sha := []byte(record.key), []byte(record.key+"_sha256")
			bound := func(raw string) func(*bolt.Bucket) error {
				return func(meta *bolt.Bucket) error {
					if err := meta.Put(key, []byte(raw)); err != nil {
						return err
					}
					return meta.Put(sha, []byte(fixtureHash([]byte(raw))))
				}
			}
			for _, tc := range []struct {
				name   string
				damage func(*bolt.Bucket) error
			}{
				{"missing binding", func(meta *bolt.Bucket) error { return meta.Put(key, []byte(record.valid)) }},
				{"missing record", func(meta *bolt.Bucket) error { return meta.Put(sha, []byte(fixtureHash([]byte(record.valid)))) }},
				{"record key is a bucket", func(meta *bolt.Bucket) error { _, err := meta.CreateBucket(key); return err }},
				{"binding key is a bucket", func(meta *bolt.Bucket) error { _, err := meta.CreateBucket(sha); return err }},
				{"bound wrong type", bound(record.invalid[0])},
				{"bound invalid count", bound(record.invalid[1])},
				{"bound invalid duration", bound(record.invalid[2])},
			} {
				t.Run(tc.name, func(t *testing.T) {
					source := filepath.Join(t.TempDir(), "source.db")
					mutateArchiveFixture(t, source, original, func(tx *bolt.Tx) error { return tc.damage(tx.Bucket([]byte("meta"))) })
					destination := filepath.Join(t.TempDir(), "restored.db")
					restored, err := board.RestoreBackup(source, destination, cfg)
					if err != nil || restored.IntegrityOK {
						t.Fatalf("transport-valid record was falsely verified: %+v, %v", restored, err)
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
					_, configErr := app.OperatingConfig("reviewer", "pilot")
					_, assessErr := app.Assess("reviewer", "pilot", id)
					if !errors.Is(configErr, board.ErrIntegrity) || !errors.Is(assessErr, board.ErrIntegrity) {
						t.Fatalf("invalid stored policy allowed a fresh decision or config: %v, %v", configErr, assessErr)
					}
					if record.key != "import_rate_policy" || tc.name != "bound invalid duration" {
						return
					}
					handler := browser.New(app, false)
					t.Cleanup(handler.Close)
					web := httptest.NewServer(handler)
					t.Cleanup(web.Close)
					client, csrf := loginSession(t, web.URL, "reviewer")
					body, status := post(t, client, web.URL+"/p/pilot/candidates/"+id+"/preview", url.Values{"_csrf": {csrf}})
					if status != http.StatusServiceUnavailable || strings.Contains(body, "Commit preview") {
						t.Fatalf("invalid rate policy did not block the actual HTTP intake: HTTP %d", status)
					}
				})
			}
		})
	}
}

func TestOperatingWritesRefuseMalformedDurableRevision(t *testing.T) {
	clock := interruptionFixtureTime
	root := t.TempDir()
	valid := filepath.Join(root, "healthy.db")
	seedHistoricalAI(t, valid, clock, nil)
	original, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}
	cfg := board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return clock }}
	for _, tc := range []struct {
		name   string
		change func(*board.Board) error
	}{
		{"custody duration", func(app *board.Board) error {
			return app.SetOperatingDuration("reviewer", "pilot", "backup-interval", 3*time.Hour)
		}},
		{"import rate", func(app *board.Board) error {
			return app.SetImportRate("reviewer", "pilot", board.ImportRatePolicy{Count: 2, Window: 3 * time.Hour})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.db")
			mutateArchiveFixture(t, path, original, func(tx *bolt.Tx) error {
				return tx.Bucket([]byte("meta")).Put([]byte("data_revision"), []byte("not-a-revision"))
			})
			app, err := board.Open(path, cfg)
			if err != nil {
				t.Fatal(err)
			}
			held, checkErr := app.AssessmentSuspended("reviewer", "pilot")
			if !held || checkErr != nil {
				_ = app.Close()
				t.Fatalf("invalid stored revision did not hold the store before a mutation: held=%v error=%v", held, checkErr)
			}
			if err := app.Revalidate("reviewer", "pilot"); !errors.Is(err, board.ErrIntegrity) {
				_ = app.Close()
				t.Fatalf("reviewer could clear a hold without repairing the revision counter: %v", err)
			}
			err = tc.change(app)
			if closeErr := app.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if !errors.Is(err, board.ErrIntegrity) {
				t.Fatalf("configuration did not report the broken revision counter: %v", err)
			}
			reopened, err := board.Open(path, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := reopened.Close(); err != nil {
					t.Error(err)
				}
			})
			durations, durationErr := reopened.OperatingConfig("reviewer", "pilot")
			policy, policyErr := reopened.ImportRate("reviewer", "pilot")
			held, holdErr := reopened.AssessmentSuspended("reviewer", "pilot")
			if result := errors.Join(durationErr, policyErr, holdErr); result != nil || !held ||
				durations["backup-interval"] != 24*time.Hour || policy != (board.ImportRatePolicy{Count: 10, Window: time.Minute}) {
				t.Fatalf("early refusal left settings or hold inconsistent on reopen: %v %+v held=%t %v", durations, policy, held, result)
			}
			audit, err := reopened.Audit("reviewer", "pilot")
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range audit {
				if entry.Action == "config.change" && entry.Outcome == "success" {
					t.Fatal("failed transaction persisted a spurious successful configuration audit")
				}
			}
		})
	}
}

func TestImportRateRollsBackWhenTerminalAuditCannotAppend(t *testing.T) {
	clock := interruptionFixtureTime
	path := filepath.Join(t.TempDir(), "rate.db")
	seedHistoricalAI(t, path, clock, nil)
	cfg := board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return clock }}
	open := func() *board.Board {
		t.Helper()
		app, err := board.Open(path, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := app.Close(); err != nil {
				t.Error(err)
			}
		})
		return app
	}
	original := board.ImportRatePolicy{Count: 3, Window: 15 * time.Second}
	app := open()
	if err := app.SetImportRate("reviewer", "pilot", original); err != nil {
		t.Fatal(err)
	}
	plan, err := app.PreviewPurge("reviewer", "pilot", nil)
	if err != nil {
		t.Fatal(err)
	}
	priorAudit, auditErr := app.Audit("reviewer", "pilot")
	if err := errors.Join(auditErr, app.Close()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mutateArchiveFixture(t, path, before, func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("audit")).SetSequence(math.MaxUint64)
	})
	app = open()
	held, err := app.AssessmentSuspended("reviewer", "pilot")
	if err != nil || held {
		t.Fatalf("terminal audit control was refused before inside-transaction work: %t, %v", held, err)
	}
	if err := app.SetImportRate("reviewer", "pilot", board.ImportRatePolicy{Count: 2, Window: time.Hour}); !errors.Is(err, board.ErrInvalid) ||
		!strings.Contains(err.Error(), "audit sequence capacity is exhausted") {
		t.Errorf("request did not reach the actual final audit append: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	app = open()
	policy, policyErr := app.ImportRate("reviewer", "pilot")
	after, revisionErr := app.PreviewPurge("reviewer", "pilot", nil)
	audit, auditErr := app.Audit("reviewer", "pilot")
	if err := errors.Join(policyErr, revisionErr, auditErr); err != nil ||
		policy != original || after.Revision != plan.Revision || !reflect.DeepEqual(priorAudit, audit) {
		t.Fatalf("terminal audit failure changed original policy, data revision or audit on reopen: %+v, %d to %d, %v",
			policy, plan.Revision, after.Revision, err)
	}
}

func TestOperatingWritesRollBackAtTerminalDataRevision(t *testing.T) {
	clock := interruptionFixtureTime
	path := filepath.Join(t.TempDir(), "terminal-revision.db")
	seedHistoricalAI(t, path, clock, nil)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mutateArchiveFixture(t, path, original, func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("meta")).Put([]byte("data_revision"), []byte("18446744073709551615"))
	})
	cfg := board.Settings{ProjectID: "pilot", ProductID: "DEMO", Clock: func() time.Time { return clock }}
	open := func() *board.Board {
		t.Helper()
		app, err := board.Open(path, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = app.Close() })
		return app
	}
	app := open()
	initial, err := app.PreviewPurge("reviewer", "pilot", nil)
	if err != nil || initial.Revision != math.MaxUint64 {
		t.Fatalf("terminal but valid data revision did not allow custody inspection: %d, %v", initial.Revision, err)
	}
	for _, update := range []func() error{
		func() error { return app.SetOperatingDuration("reviewer", "pilot", "backup-interval", 5*time.Hour) },
		func() error {
			return app.SetImportRate("reviewer", "pilot", board.ImportRatePolicy{Count: 2, Window: time.Minute})
		},
	} {
		if err := update(); !errors.Is(err, board.ErrInvalid) || !strings.Contains(err.Error(), "storage counter capacity is exhausted") {
			t.Errorf("valid setting did not reach the terminal revision advance: %v", err)
		}
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	app = open()
	rate, rateErr := app.ImportRate("reviewer", "pilot")
	config, configErr := app.OperatingConfig("reviewer", "pilot")
	after, revisionErr := app.PreviewPurge("reviewer", "pilot", nil)
	if err := errors.Join(rateErr, configErr, revisionErr); err != nil || after.Revision != initial.Revision ||
		rate != (board.ImportRatePolicy{Count: 10, Window: time.Minute}) || config["backup-interval"] != 24*time.Hour {
		t.Fatalf("terminal revision did not preserve both settings on reopen: rate=%+v interval=%v revision=%d: %v",
			rate, config["backup-interval"], after.Revision, err)
	}
	audit, err := app.Audit("reviewer", "pilot")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range audit {
		if event.Action == "config.change" && event.Outcome == "success" {
			t.Fatal("uncommitted configuration change left a successful audit event")
		}
	}
}
