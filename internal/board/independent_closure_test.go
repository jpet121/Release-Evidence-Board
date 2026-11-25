package board_test

import (
	"bytes"
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
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"release-evidence-board/internal/board"
	"release-evidence-board/internal/browser"
)

func (e *independentEnvironment) settings() board.Settings {
	return board.Settings{ProjectID: independentProject, ProductID: "DEMO", Clock: func() time.Time { return e.now }}
}

// Only the detached backup is edited. The restored copy is given the corrected
// outer checksum so the check reaches the persisted records and references.
func (e *independentEnvironment) faultedBackup(mutate func(*bolt.Tx) error) (string, string) {
	t := e.t
	t.Helper()
	backup, err := e.b.Backup(independentOwner, independentProject, t.TempDir())
	independentRequire(t, err)
	detached, err := bolt.Open(backup.Path, 0o600, &bolt.Options{Timeout: time.Second})
	independentRequire(t, err)
	edit := detached.Update(mutate)
	independentRequire(t, detached.Close())
	independentRequire(t, edit)
	raw, err := os.ReadFile(backup.Path)
	independentRequire(t, err)
	changedSHA := independentSHA(raw)
	if changedSHA == backup.SHA256 {
		t.Fatal("detached fixture mutation did not change the outer checksum")
	}
	independentRequire(t, os.WriteFile(backup.Path+".sha256", []byte(changedSHA+"\n"), 0o600))
	live := filepath.Join(t.TempDir(), "detached-live.db")
	independentRequire(t, os.WriteFile(live, raw, 0o600))
	return backup.Path, live
}

func displaceDetachedRecord(tx *bolt.Tx, bucketName, key, wrongKey string) error {
	bucket := tx.Bucket([]byte(bucketName))
	value := bytes.Clone(bucket.Get([]byte(key)))
	if len(value) == 0 || bucket.Get([]byte(wrongKey)) != nil {
		return fmt.Errorf("fixture's source or destination key is inconsistent in %s", bucketName)
	}
	if err := bucket.Put([]byte(wrongKey), value); err != nil {
		return err
	}
	return bucket.Delete([]byte(key))
}

func (e *independentEnvironment) checkRetainedKeyOrReceipt() {
	t := e.t
	first := e.commit("a")
	e.commit("b")
	frozen, err := e.b.Freeze(independentOwner, independentProject, e.candidate.ID)
	independentRequire(t, err)
	_, archived, err := e.b.ReportHTML(independentOwner, independentProject, frozen.ID)
	independentRequire(t, err)
	contents := independentBaseline(t, e.scenario.Setup.ApprovedBaseline.raw(t))
	contents.ID = "Unapproved"
	draft, err := e.b.CreateDraft(independentAuthor, independentProject, contents, board.DraftOptions{})
	independentRequire(t, err)
	expected := independentDecode[struct{ StorageFault, KeyDomain string }](t, e.scenario.Expected)
	const displaced = "wrong-fixture-key"
	backup, live := e.faultedBackup(func(tx *bolt.Tx) error {
		switch expected.StorageFault {
		case "receipt moved under unrelated primary key":
			return displaceDetachedRecord(tx, "receipts", first.Hash, displaced)
		case "run moved under unrelated primary key":
			return displaceDetachedRecord(tx, "runs", "a", displaced)
		case "candidate moved under unrelated primary key":
			return displaceDetachedRecord(tx, "candidates", e.candidate.ID, displaced)
		case "draft moved under unrelated primary key":
			return displaceDetachedRecord(tx, "drafts", draft.ID, displaced)
		case "retained source row provenance changed":
			bucket := tx.Bucket([]byte("runs"))
			var run board.Run
			if err := json.Unmarshal(bucket.Get([]byte("a")), &run); err != nil {
				return err
			}
			if len(run.Rows) != 1 || run.Rows[0].ResultID() != "A1" {
				return errors.New("fixture's intended row is missing")
			}
			run.Rows[0].LogicalRow++
			return replaceDetachedJSON(bucket, "a", run)
		}
		changed := first
		switch expected.StorageFault {
		case "receipt ID changed":
			changed.ID = "I-" + strings.Repeat("a", 64)
		case "receipt hash changed":
			changed.Hash = strings.Repeat("b", 64)
		case "receipt ID emptied":
			changed.ID = ""
		default:
			return fmt.Errorf("unknown fixture fault %q", expected.StorageFault)
		}
		return replaceDetachedJSON(tx.Bucket([]byte("receipts")), first.Hash, changed)
	})
	app, err := board.Open(live, e.settings())
	independentRequire(t, err)
	defer app.Close()
	switch expected.KeyDomain {
	case "receipt":
		value, replayErr := app.CommitImport(independentAuthor, independentProject, e.previews["a"].Token, e.raw("a"))
		independentEqual(t, "damaged original receipt rejects exact stale-preview replay", errors.Is(replayErr, board.ErrIntegrity), true)
		independentEqual(t, "damaged receipt never returned as a successful record", value.ID, "")
	case "run":
		_, err = app.Assess(independentOwner, independentProject, e.candidate.ID)
		independentEqual(t, "missing canonical run key detected by live assessment", errors.Is(err, board.ErrIntegrity), true)
	case "candidate":
		_, _, _, err = app.Candidate(independentOwner, independentProject, displaced)
		independentEqual(t, "moved candidate rejects noncanonical read", errors.Is(err, board.ErrIntegrity), true)
	case "draft":
		_, err = app.Draft(independentOwner, independentProject, displaced)
		independentEqual(t, "moved draft rejects noncanonical read", errors.Is(err, board.ErrIntegrity), true)
	}
	health, err := app.Health(independentOwner, independentProject)
	independentRequire(t, err)
	independentEqual(t, "detection latches integrity", health.Hold, true)
	_, err = app.Freeze(independentOwner, independentProject, e.candidate.ID)
	independentEqual(t, "new report fails closed after detection", errors.Is(err, board.ErrIntegrity), true)
	_, html, err := app.ReportHTML(independentOwner, independentProject, frozen.ID)
	independentRequire(t, err)
	independentEqual(t, "historical HTML bytes remain readable through the hold", html, archived)
	inspectDetachedStore(t, live, func(tx *bolt.Tx, _ []byte) {
		independentEqual(t, "no duplicate or replacement receipt is created", tx.Bucket([]byte("receipts")).Stats().KeyN, 2)
		intact := false
		independentRequire(t, tx.Bucket([]byte("csv_bytes")).ForEach(func(_, data []byte) error {
			intact = intact || bytes.Equal(data, e.raw("a"))
			return nil
		}))
		independentEqual(t, "original CSV byte value survives primary key fault", intact, true)
	})
	e.assertFaultyRestore(backup)
}

func (e *independentEnvironment) assertFaultyRestore(backup string) {
	t := e.t
	t.Helper()
	restoredPath := filepath.Join(t.TempDir(), "restored-fault.db")
	result, err := board.RestoreBackup(backup, restoredPath, e.settings())
	independentRequire(t, err)
	independentEqual(t, "valid outer checksum cannot establish integrity of damaged records", result.IntegrityOK, false)
	app, err := board.Open(restoredPath, e.settings())
	independentRequire(t, err)
	defer app.Close()
	independentEqual(t, "damaged store cannot be reviewer-revalidated", errors.Is(app.Revalidate(independentOwner, independentProject), board.ErrIntegrity), true)
	_, err = app.Assess(independentOwner, independentProject, e.candidate.ID)
	independentEqual(t, "restore hold prevents live status", errors.Is(err, board.ErrIntegrity), true)
}

func (e *independentEnvironment) checkFrozenCompanionMetadata() {
	t := e.t
	e.commit("failed")
	e.commit("passing")
	frozen, err := e.b.Freeze(independentOwner, independentProject, e.candidate.ID)
	independentRequire(t, err)
	_, archived, err := e.b.ReportHTML(independentOwner, independentProject, frozen.ID)
	independentRequire(t, err)
	if !bytes.Contains(archived, []byte("Historical assessment · BLOCKED")) || !bytes.Contains(archived, []byte(independentMID)) {
		t.Fatal("literal expected historical status and assessment time did not appear in the originally frozen HTML")
	}
	fault := independentDecode[struct{ StorageFault string }](t, e.scenario.Expected).StorageFault
	const alteredTime = "2026-11-18T06:00:00Z"
	backup, live := e.faultedBackup(func(tx *bolt.Tx) error {
		changed := frozen
		if fault == "status" {
			changed.Status = "SUFFICIENT"
		} else {
			changed.AssessedAt = independentTime(t, alteredTime)
		}
		// Change one field in the existing object instead of recreating its
		// schema. This also preserves any separately added integrity metadata.
		bucket := tx.Bucket([]byte("reports"))
		var stored map[string]json.RawMessage
		if err := json.Unmarshal(bucket.Get([]byte(frozen.ID)), &stored); err != nil {
			return err
		}
		field, value := "Status", any(changed.Status)
		if fault == "assessment time" {
			field, value = "AssessedAt", changed.AssessedAt
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		stored[field] = raw
		return replaceDetachedJSON(bucket, frozen.ID, stored)
	})
	app, err := board.Open(live, e.settings())
	independentRequire(t, err)
	defer app.Close()
	client, address := independentHTTP(t, app)
	_, history := independentBrowserGet(t, client, address+"/p/pilot/candidates/"+e.candidate.ID+"/reports")
	if bytes.Contains(history, []byte("SUFFICIENT")) || bytes.Contains(history, []byte(alteredTime)) {
		t.Error("report history advertised a changed metadata field")
	}
	if !bytes.Contains(bytes.ToLower(history), []byte("integrity")) {
		t.Error("report history did not disclose uncertain integrity")
	}
	response, direct := independentBrowserGet(t, client, address+"/p/pilot/reports/"+frozen.ID)
	independentEqual(t, "direct historical HTML remains byte-identical", direct, archived)
	independentEqual(t, "direct historical HTML succeeds read-only", response.StatusCode, http.StatusOK)
	if response.Header.Get("X-REB-Integrity-Warning") == "" {
		t.Error("direct historical HTML did not expose an integrity warning header")
	}
	e.assertFaultyRestore(backup)
}

func independentHTTP(t *testing.T, app *board.Board) (*http.Client, string) {
	t.Helper()
	handler := browser.New(app, false)
	t.Cleanup(func() { handler.Close() })
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	independentRequire(t, err)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	_, page := independentBrowserGet(t, client, server.URL+"/login")
	root, _ := offlineParse(t, page)
	csrf := offlineOne(t, "login CSRF field", root.matching(func(node *offlineElement) bool { return node.attrs["name"] == "_csrf" })).attrs["value"]
	response, err := client.PostForm(server.URL+"/login", url.Values{"actor": {independentOwner}, "password": {"owner-synthetic-password"}, "_csrf": {csrf}})
	independentRequire(t, err)
	defer response.Body.Close()
	if !strings.HasSuffix(response.Request.URL.Path, "/p/pilot") {
		t.Fatalf("fixture login did not reach its protected project: %s", response.Request.URL.Path)
	}
	return client, server.URL
}

func independentBrowserGet(t *testing.T, client *http.Client, url string) (*http.Response, []byte) {
	t.Helper()
	response, err := client.Get(url)
	independentRequire(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	independentRequire(t, err)
	return response, data
}

func (e *independentEnvironment) checkPreviewHoldRace() {
	t := e.t
	independentRequire(t, e.b.Close())
	var armed atomic.Bool
	var ticks atomic.Int32
	holdInsideWrite, previewStarted, permitHoldCommit := make(chan struct{}), make(chan struct{}), make(chan struct{})
	settings := e.settings()
	settings.Clock = func() time.Time {
		if armed.Load() {
			switch ticks.Add(1) {
			case 1:
				close(holdInsideWrite)
				<-permitHoldCommit
			case 2:
				close(previewStarted)
			}
		}
		return e.now
	}
	var err error
	e.b, err = board.Open(e.path, settings)
	independentRequire(t, err)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(permitHoldCommit) }) }
	defer release()
	armed.Store(true)
	holdDone := make(chan error, 1)
	go func() { holdDone <- e.b.SuspendAssessments(independentOwner, independentProject) }()
	select {
	case <-holdInsideWrite:
	case <-time.After(5 * time.Second):
		t.Fatal("operator did not reach its held bbolt write transaction")
	}
	type result struct {
		preview board.Preview
		err     error
	}
	previewDone := make(chan result, 1)
	raw := e.raw("a")
	go func() {
		p, err := e.b.PreviewImport(independentAuthor, independentProject, e.candidate.ID, raw)
		previewDone <- result{preview: p, err: err}
	}()
	select {
	case <-previewStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("preview did not begin while hold was still uncommitted")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !queuedPreviewWrite() {
		if time.Now().After(deadline) {
			t.Fatal("preview never queued a real bbolt writer after reading the pre-hold snapshot")
		}
		time.Sleep(5 * time.Millisecond)
	}
	release()
	select {
	case err := <-holdDone:
		independentRequire(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("operator did not complete suspension")
	}
	var observation result
	select {
	case observation = <-previewDone:
	case <-time.After(5 * time.Second):
		t.Fatal("preview did not return after the hold committed")
	}
	independentEqual(t, "durable hold wins preview issuance", errors.Is(observation.err, board.ErrIntegrity), true)
	independentEqual(t, "no new confirm token issued", observation.preview.Token == "", true)
	inspectDetachedStore(t, e.path, func(tx *bolt.Tx, _ []byte) {
		independentEqual(t, "no new binding stored after hold", tx.Bucket([]byte("previews")).Stats().KeyN, 0)
		independentEqual(t, "no new staged CSV stored after hold", tx.Bucket([]byte("preview_csv")).Stats().KeyN, 0)
	})
	t.Log("real pre-hold read completed and preview writer queued before the operator committed the hold")
}

func queuedPreviewWrite() bool {
	raw := make([]byte, 256*1024)
	size := runtime.Stack(raw, true)
	for _, stack := range strings.Split(string(raw[:size]), "\n\n") {
		if strings.Contains(stack, "(*Board).PreviewImport") && strings.Contains(stack, "(*DB).beginRWTx") {
			return true
		}
	}
	return false
}

func (e *independentEnvironment) checkMixedLedger() {
	t := e.t
	newPair, _, err := e.b.CreateCandidate(independentAuthor, independentProject, board.CandidateInput{
		Manifest: independentManifest(t, e.scenario.Setup.SecondaryManifest.raw(t)), BaselineDigest: e.baseline.Descriptor.Digest, DisplayVersion: "1.0.0"})
	independentRequire(t, err)
	p := e.preview("mixedNew", e.candidate.ID)
	independentEqual(t, "both candidate projections are shown", len(p.Projected), 2)
	for _, projected := range p.Projected {
		independentEqual(t, "projection shows the final product-wide ledger", projected.LedgerHighWater, uint64(2))
	}
	receipt := e.confirm("mixedNew")
	independentEqual(t, "receipt agrees with both projected ledger values", receipt.LedgerHighWater, uint64(2))
	for id, sequence := range map[string]uint64{e.candidate.ID: 1, newPair.ID: 2} {
		status := e.assess(id, "")
		independentEqual(t, "both exact pairs separately pass", status.Status, "SUFFICIENT")
		runs := independentBehavior(t, status, "BEH1").Runs
		if len(runs) != 1 {
			t.Fatal("expected each pair to retain exactly one newly committed run")
		}
		independentEqual(t, "ASCII run order assigns product sequence", runs[0].Sequence, sequence)
	}
}

func (e *independentEnvironment) checkOptionalizedAlias() {
	t := e.t
	e.commit("history")
	e.addAccount("reducer", board.Reviewer)
	e.addAccount("independent", board.Reviewer)
	contents := independentBaseline(t, e.scenario.Setup.ApprovedBaseline.raw(t))
	draft, err := e.b.CreateDraft("reducer", independentProject, contents, board.DraftOptions{FromBaseline: e.baseline.Descriptor.Digest})
	independentRequire(t, err)
	newContents := draft.Baseline
	newRequired := newContents.Behaviors[0]
	newContents.Behaviors[0].Required = false
	newRequired.ID = "NEW-REQUIRED"
	newRequired.Policy.ID = "NEW-POLICY"
	newRequired.Policy.MinPassBasisPoints = 5000
	newRequired.Tests = append([]board.TestPlan(nil), newRequired.Tests...)
	newRequired.Tests[0].ID = "NEW-T1"
	newRequired.Tests[1].ID = "NEW-T2"
	distinct := independentDecode[struct{ NewBehaviorIntentDistinct bool }](t, e.scenario.Expected).NewBehaviorIntentDistinct
	if distinct {
		newRequired.Tests[0].ExpectedOutcome = "Access rejection emits a new signed diagnostic"
	}
	newContents.Behaviors = append(newContents.Behaviors, newRequired)
	edited, err := e.b.SaveDraft("reducer", independentProject, draft.ID, draft.Revision, newContents)
	independentRequire(t, err)
	approved, err := e.b.Approve("reducer", independentProject, edited.ID, edited.Revision)
	if distinct {
		independentRequire(t, err)
		independentEqual(t, "genuinely distinct required behavior is authorized", approved.Approver, "reducer")
	} else {
		independentEqual(t, "equal-intent required alias still needs independent approval", errors.Is(err, board.ErrForbidden), true)
		approved, err = e.b.Approve("independent", independentProject, edited.ID, edited.Revision)
		independentRequire(t, err)
		independentEqual(t, "independent reviewer recorded", approved.Approver, "independent")
		affected := false
		for _, change := range approved.Thresholds {
			if change.CandidateID == e.candidate.ID && change.Before == 10000 && change.After == 5000 {
				affected = true
				independentEqual(t, "named reduction actor retained", change.ReducingActors, []string{"reducer"})
			}
		}
		independentEqual(t, "approval records the exact prior candidate and changed threshold", affected, true)
	}
	independentEqual(t, "retained original failure still blocks its candidate", e.assess(e.candidate.ID, "").Status, "BLOCKED")
}

func (e *independentEnvironment) checkCanonicalBoundaries() {
	t := e.t
	expect := independentDecode[struct{ Phase, Descriptor, CanonicalSHA256 string }](t, e.scenario.Expected)
	input := e.scenario.Setup.DescriptorInput.raw(t)
	if expect.Phase == "invalid UTF-8 application label" {
		manifest := independentManifest(t, input)
		manifest.ApplicationVersion = string([]byte{0xed, 0xa0, 0x80})
		_, err := manifest.Descriptor()
		independentEqual(t, "UTF-8 encoding of a surrogate is rejected", err != nil, true)
		return
	}
	baseline := independentBaseline(t, input)
	behavior := &baseline.Behaviors[0]
	switch expect.Phase {
	case "exact scalar and ASCII limits":
	case "overlong multibyte behavior text":
		behavior.Text += "🙂"
	case "overlong multibyte expected outcome":
		behavior.Tests[0].ExpectedOutcome += "🙂"
	case "zero baseline revision":
		baseline.Revision = 0
	case "zero policy revision":
		behavior.Policy.Revision = 0
	case "optional behavior without tests":
		baseline.Behaviors = append(baseline.Behaviors, board.Behavior{ID: "OPTIONAL", Revision: 1, Required: false, Text: "Distinct optional behavior",
			Policy: board.Policy{ID: "OPTIONAL-POLICY", Revision: 1, MinPassBasisPoints: 5000, MaxAgeHours: 24}})
	default:
		t.Fatalf("unknown descriptor boundary fixture %q", expect.Phase)
	}
	descriptor, err := baseline.Descriptor()
	if expect.Descriptor == "ACCEPTED" {
		independentRequire(t, err)
		independentEqual(t, "literal Unicode scalar and ASCII boundaries preserved", descriptor.Text, string(input))
		independentEqual(t, "independent literal canonical SHA-256", descriptor.Digest, expect.CanonicalSHA256)
	} else {
		independentEqual(t, "invalid descriptor receives a validation error", err != nil, true)
	}
	approved, drafts, catalogError := e.b.Baselines(independentOwner, independentProject)
	independentRequire(t, catalogError)
	independentEqual(t, "descriptor-only action leaves catalog empty", len(approved)+len(drafts), 0)
}
