package board_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"release-evidence-board/internal/board"
	"release-evidence-board/internal/browser"
)

type backupCustodyFixture struct {
	app       *board.Board
	original  board.Backup
	candidate board.Candidate
	baseline  board.ApprovedBaseline
	receipt   board.Receipt
	report    board.Report
	html      []byte
}

func newBackupCustodyFixture(t *testing.T) backupCustodyFixture {
	t.Helper()
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	candidate := fixtureCandidate(t, app, vectorBaseline())
	_, _, baseline, err := app.Candidate(supervisor, project, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	stamp := board.FormatUTC(fixtureTime)
	raw := csvBytes(t, resultRow(candidate, "T1", "1", "CUSTODY-PASS", "CUSTODY-RESULT", stamp, stamp, "PASS"))
	receipt := importBytes(t, app, candidate.ID, raw)
	report, err := app.Freeze(supervisor, project, candidate.ID)
	if err != nil || report.Status != board.Sufficient {
		t.Fatalf("intact source did not freeze a genuine sufficient report: %v", err)
	}
	_, html, err := app.ReportHTML(supervisor, project, report.ID)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := app.Backup(supervisor, project, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return backupCustodyFixture{app, backup, candidate, baseline, receipt, report, html}
}

func custodyDigest(data []byte) string {
	value := sha256.Sum256(data)
	return hex.EncodeToString(value[:])
}

func (fx backupCustodyFixture) detachedFault(t *testing.T, fault func(*bolt.Tx) error) string {
	t.Helper()
	intact, err := os.ReadFile(fx.original.Path)
	if err != nil || custodyDigest(intact) != fx.original.SHA256 {
		t.Fatalf("source backup changed before the controlled test: %v", err)
	}
	path := filepath.Join(t.TempDir(), "detached-backup.db")
	if err := os.WriteFile(path, intact, 0600); err != nil {
		t.Fatal(err)
	}
	copyDB, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(copyDB.Update(fault), copyDB.Close()); err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".sha256", []byte(custodyDigest(bytes)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (fx backupCustodyFixture) verifyCorruptRestore(t *testing.T, source string) *board.Board {
	t.Helper()
	target := filepath.Join(t.TempDir(), "held-recovered.db")
	settings := board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return fixtureTime }}
	record, err := board.RestoreBackup(source, target, settings)
	if err != nil || record.IntegrityOK {
		t.Fatalf("restored changed custody as verified: integrity=%t error=%v", record.IntegrityOK, err)
	}
	app, err := board.Open(target, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	held, err := app.AssessmentSuspended(supervisor, project)
	if err != nil || !held {
		t.Fatalf("restored corruption lacks its public hold: %v", err)
	}
	if _, err := app.Assess(supervisor, project, fx.candidate.ID); !errors.Is(err, board.ErrIntegrity) {
		t.Fatalf("corrupt recovery issued a release decision: %v", err)
	}
	if _, err := app.Freeze(supervisor, project, fx.candidate.ID); !errors.Is(err, board.ErrIntegrity) {
		t.Fatalf("corrupt recovery issued fresh export: %v", err)
	}
	if err := app.Revalidate(supervisor, project); !errors.Is(err, board.ErrIntegrity) {
		t.Fatalf("reviewer cleared the corrupt recovery hold: %v", err)
	}
	_, html, err := app.ReportHTML(supervisor, project, fx.report.ID)
	if err != nil || !bytes.Equal(html, fx.html) {
		t.Fatalf("corruption damaged intact historical frozen bytes: %v", err)
	}
	intact, err := os.ReadFile(fx.original.Path)
	if err != nil || custodyDigest(intact) != fx.original.SHA256 {
		t.Fatalf("detached fault changed the genuine source backup: %v", err)
	}
	_, current, err := fx.app.ReportHTML(supervisor, project, fx.report.ID)
	if err != nil || !bytes.Equal(current, fx.html) {
		t.Fatalf("detached fault changed the genuine source store: %v", err)
	}
	return app
}

func TestFollowupEveryApprovedDescriptorFieldRejectsDirectChange(t *testing.T) {
	fx := newBackupCustodyFixture(t)
	settings := board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return fixtureTime }}
	control, err := board.RestoreBackup(fx.original.Path, filepath.Join(t.TempDir(), "positive.db"), settings)
	if err != nil || !control.IntegrityOK {
		t.Fatalf("original transport and metadata did not restore correctly: %v", err)
	}
	for i, item := range []struct {
		field string
		path  []int
		value any
	}{
		{"version tag", []int{0}, "reb-baseline/9"},
		{"product_id", []int{1}, "DEMO-FOREIGN"},
		{"baseline_id", []int{2}, "B-DIFFERENT"},
		{"baseline_revision", []int{3}, 2},
		{"behaviors", []int{4}, []any{}},
		{"behavior_id", []int{4, 0, 0}, "BEH-DIFFERENT"},
		{"behavior_revision", []int{4, 0, 1}, 2},
		{"required", []int{4, 0, 2}, false},
		{"text", []int{4, 0, 3}, "New unsanctioned text"},
		{"policy_id", []int{4, 0, 4}, "POL-DIFFERENT"},
		{"policy_revision", []int{4, 0, 5}, 2},
		{"min_pass_basis_points", []int{4, 0, 6}, 5000},
		{"max_age_hours", []int{4, 0, 7}, 48},
		{"tests", []int{4, 0, 8}, []any{}},
		{"test_id", []int{4, 0, 8, 0, 0}, "T-DIFFERENT"},
		{"expected_outcome", []int{4, 0, 8, 0, 1}, "New unsanctioned expected behavior"},
		{"must_pass", []int{4, 0, 8, 0, 2}, false},
		{"planned_attempts", []int{4, 0, 8, 0, 3}, 2},
	} {
		id := fmt.Sprintf("AC-002-02.P%03d", i+1)
		t.Run(id, func(t *testing.T) {
			file := fx.detachedFault(t, func(tx *bolt.Tx) error {
				var retained board.ApprovedBaseline
				key := []byte(fx.baseline.Descriptor.Digest)
				baselines := tx.Bucket([]byte("baselines"))
				if err := json.Unmarshal(baselines.Get(key), &retained); err != nil {
					return err
				}
				var wire []any
				if err := json.Unmarshal([]byte(retained.Descriptor.Text), &wire); err != nil {
					return err
				}
				original, err := json.Marshal(wire)
				if err != nil || string(original) != retained.Descriptor.Text {
					return fmt.Errorf("fixture C01 source was changed by unrelated serialization: %w", err)
				}
				field := wire
				for _, step := range item.path[:len(item.path)-1] {
					field = field[step].([]any)
				}
				field[item.path[len(item.path)-1]] = item.value
				changed, err := json.Marshal(wire)
				if err != nil || bytes.Equal(changed, original) {
					return fmt.Errorf("fixture had no single-field C01 change: %w", err)
				}
				retained.Descriptor.Text = string(changed)
				edit, err := json.Marshal(retained)
				if err != nil {
					return err
				}
				checksums := tx.Bucket([]byte("baseline_metadata_sha256"))
				return errors.Join(baselines.Put(key, edit), checksums.Put(key, []byte(custodyDigest(edit))))
			})
			fx.verifyCorruptRestore(t, file)
			recordApprovalInstance(t, id, map[string]any{"descriptorField": item.field}, "Rejected")
		})
	}
}

func TestFollowupCorruptedAndMissingSourceCannotClearRestoreHold(t *testing.T) {
	fx := newBackupCustodyFixture(t)
	for _, item := range []struct{ id, defect string }{
		{"AC-014-02.P001", "corrupt evidence bytes"},
		{"AC-014-02.P002", "missing evidence file"},
	} {
		t.Run(item.id, func(t *testing.T) {
			file := fx.detachedFault(t, func(tx *bolt.Tx) error {
				sources := tx.Bucket([]byte("csv_bytes"))
				key := []byte(fx.receipt.Hash)
				original := sources.Get(key)
				if !bytes.Contains(original, []byte("CUSTODY-RESULT")) {
					return errors.New("fixture does not contain the exact original evidence")
				}
				if item.id == "AC-014-02.P002" {
					return sources.Delete(key)
				}
				changes := bytes.Replace(original, []byte("CUSTODY-RESULT"), []byte("CHANGED-RESULT"), 1)
				return sources.Put(key, changes)
			})
			restored := fx.verifyCorruptRestore(t, file)
			assertRestoredWarning(t, restored)
			recordApprovalInstance(t, item.id, map[string]any{"backupDefect": item.defect}, "Integrity warning; new assessments/exports blocked")
		})
	}
}

func assertRestoredWarning(t *testing.T, app *board.Board) {
	t.Helper()
	handler := browser.New(app, false)
	t.Cleanup(handler.Close)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	login, err := client.Get(server.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(login.Body)
	if err := errors.Join(err, login.Body.Close()); err != nil {
		t.Fatal(err)
	}
	csrf := regexp.MustCompile(`name="_csrf" value="([^"]+)"`).FindSubmatch(body)
	if login.StatusCode != http.StatusOK || len(csrf) != 2 {
		t.Fatal("restored store did not expose a real authenticated entry")
	}
	verified, err := client.PostForm(server.URL+"/login", url.Values{
		"actor": {supervisor}, "password": {testPassword}, "_csrf": {string(csrf[1])},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := verified.Body.Close(); err != nil || verified.StatusCode != http.StatusSeeOther {
		t.Fatalf("restored reviewer could not authenticate: %v", err)
	}
	view, err := client.Get(server.URL + "/p/pilot")
	if err != nil {
		t.Fatal(err)
	}
	html, err := io.ReadAll(view.Body)
	if err := errors.Join(err, view.Body.Close()); err != nil {
		t.Fatal(err)
	}
	if view.StatusCode != http.StatusOK || view.Header.Get("X-REB-Integrity-Warning") != "assessment-suspended" ||
		!strings.Contains(string(html), "Evidence integrity warning. New assessments and exports are suspended") {
		t.Fatal("held workspace did not disclose the restore integrity warning")
	}
}

func TestFollowupAbortedImportStagingExpiresAfterRealRestart(t *testing.T) {
	now := fixtureTime
	store := filepath.Join(t.TempDir(), "staged-csv.db")
	settings := board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return now }}
	app, err := board.Open(store, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if app != nil {
			if err := app.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	if err := app.Bootstrap(supervisor, testPassword); err != nil {
		t.Fatal(err)
	}
	candidate := fixtureCandidate(t, app, vectorBaseline())
	stamp := board.FormatUTC(now)
	good := csvBytes(t, resultRow(candidate, "T1", "1", "STAGED-PASS", "KEPT-RESULT", stamp, stamp, "PASS"))
	receipt := importBytes(t, app, candidate.ID, good)
	adverse := csvBytes(t, resultRow(candidate, "T1", "1", "ABANDONED-RUN", "ABANDONED-RESULT", stamp, stamp, "FAIL"))
	preview, err := app.PreviewImport(supervisor, project, candidate.ID, adverse)
	if err != nil || preview.Token == "" || len(preview.Projected) != 1 || preview.Projected[0].Status != board.Blocked {
		t.Fatalf("real staged CSV did not project a meaningful changed decision: %v", err)
	}
	_ = newCustodySuccessor(t, app, candidate, "a")
	if _, err := app.CommitImport(supervisor, project, preview.Token, adverse); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("stale database transaction admitted or misclassified its staged upload: %v", err)
	}
	check := func(current *board.Board) {
		t.Helper()
		assessment, err := current.Assess(supervisor, project, candidate.ID)
		if err != nil || assessment.Status != board.Sufficient || assessment.LedgerHighWater != 1 ||
			len(assessment.Behaviors) != 1 || len(assessment.Behaviors[0].Runs) != 1 ||
			assessment.Behaviors[0].CurrentRunID != "STAGED-PASS" {
			t.Fatalf("refused transaction changed the visible committed ledger: %v", err)
		}
		data, err := current.SourceCSV(supervisor, project, receipt.ID)
		if err != nil || !bytes.Equal(data, good) {
			t.Fatalf("staging failure changed prior committed source bytes: %v", err)
		}
	}
	check(app)
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	app = nil
	checkDetachedStaging(t, store, preview.Token, adverse)

	now = now.Add(31 * time.Minute)
	app, err = board.Open(store, settings)
	if err != nil {
		t.Fatal(err)
	}
	check(app)
	if _, err := app.CommitImport(supervisor, project, preview.Token, adverse); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("startup kept the expired failed staging token usable: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	app = nil
	checkDetachedStaging(t, store, preview.Token, nil)
	t.Log("AC-011-03 projection: rejected transaction retained no committed run; restart removed exact staged inline CSV and token")
}

func checkDetachedStaging(t *testing.T, store, token string, expected []byte) {
	t.Helper()
	db, err := bolt.Open(store, 0600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = db.View(func(tx *bolt.Tx) error {
		key := []byte(token)
		binding := tx.Bucket([]byte("previews")).Get(key)
		data := tx.Bucket([]byte("preview_csv")).Get(key)
		if (binding != nil) != (expected != nil) || !bytes.Equal(data, expected) {
			return errors.New("stored preview and inline CSV did not reflect the recorded custody state")
		}
		return nil
	})
	if err := errors.Join(err, db.Close()); err != nil {
		t.Fatal(err)
	}
}
