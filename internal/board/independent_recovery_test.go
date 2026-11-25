package board_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"release-evidence-board/internal/board"
)

type physicalStoreState struct {
	transaction int
	sha256      string
}

func detachedPhysicalState(t *testing.T, livePath string) physicalStoreState {
	t.Helper()
	var state physicalStoreState
	inspectDetachedStore(t, livePath, func(tx *bolt.Tx, raw []byte) {
		state = physicalStoreState{transaction: tx.ID(), sha256: independentSHA(raw)}
	})
	return state
}

func inspectDetachedStore(t *testing.T, livePath string, record func(*bolt.Tx, []byte)) {
	t.Helper()
	raw, err := os.ReadFile(livePath)
	independentRequire(t, err)
	path := filepath.Join(t.TempDir(), "metadata-copy.db")
	independentRequire(t, os.WriteFile(path, raw, 0o600))
	copyDB, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	independentRequire(t, err)
	defer copyDB.Close()
	independentRequire(t, copyDB.View(func(tx *bolt.Tx) error { record(tx, raw); return nil }))
}

func (e *independentEnvironment) checkCorruptBackup() {
	t := e.t
	e.commit("a")
	frozen, err := e.b.Freeze(independentOwner, independentProject, e.candidate.ID)
	independentRequire(t, err)
	_, frozenBefore, err := e.b.ReportHTML(independentOwner, independentProject, frozen.ID)
	independentRequire(t, err)
	backup, err := e.b.Backup(independentOwner, independentProject, t.TempDir())
	independentRequire(t, err)

	// The only mutation is to a disposable backup. Locate the known fixture
	// value by content and keep all other database records structurally intact.
	staging, err := bolt.Open(backup.Path, 0o600, &bolt.Options{Timeout: time.Second})
	independentRequire(t, err)
	original := e.raw("a")
	changed := bytes.Clone(original)
	changed[len(changed)-1] = '\r'
	matched := 0
	changeErr := staging.Update(func(tx *bolt.Tx) error {
		return tx.ForEach(func(_ []byte, bucket *bolt.Bucket) error {
			var keyCopy []byte
			err := bucket.ForEach(func(key, value []byte) error {
				if bytes.Equal(value, original) {
					keyCopy = bytes.Clone(key)
					matched++
				}
				return nil
			})
			if err != nil || keyCopy == nil {
				return err
			}
			return bucket.Put(keyCopy, changed)
		})
	})
	independentRequire(t, staging.Close())
	independentRequire(t, changeErr)
	if matched != 1 {
		t.Fatalf("expected one original CSV in the disposable backup, found %d", matched)
	}

	restoredPath := filepath.Join(t.TempDir(), "restored.db")
	settings := board.Settings{ProjectID: independentProject, ProductID: "DEMO", Clock: func() time.Time { return e.now }}
	restore, err := board.RestoreBackup(backup.Path, restoredPath, settings)
	independentRequire(t, err)
	independentEqual(t, "corrupt original causes integrity warning", restore.IntegrityOK, false)
	restored, err := board.Open(restoredPath, settings)
	independentRequire(t, err)
	defer restored.Close()
	_, frozenAfter, err := restored.ReportHTML(independentOwner, independentProject, frozen.ID)
	independentRequire(t, err)
	independentEqual(t, "frozen report stays available read only", frozenAfter, frozenBefore)
	_, err = restored.Freeze(independentOwner, independentProject, e.candidate.ID)
	independentEqual(t, "corrupt restore blocks new report", errors.Is(err, board.ErrIntegrity), true)
	_, err = restored.Assess(independentOwner, independentProject, e.candidate.ID)
	independentEqual(t, "corrupt restore blocks live assessment", errors.Is(err, board.ErrIntegrity), true)
	if err := restored.Revalidate(independentOwner, independentProject); err == nil {
		t.Error("reviewer revalidation accepted corrupt original bytes")
	}
	_, err = restored.Freeze(independentOwner, independentProject, e.candidate.ID)
	independentEqual(t, "hold persists after incomplete repair", errors.Is(err, board.ErrIntegrity), true)
}

func (e *independentEnvironment) checkDetachedIntegrity() {
	t := e.t
	beforeHistory := e.commit("history")
	beforeCurrent := e.commit("current")
	initial := e.assess(e.candidate.ID, "")
	expect := independentDecode[struct {
		Initial      independentStatus
		StorageFault string
	}](t, e.scenario.Expected)
	e.checkStatus(initial, expect.Initial)
	frozen, err := e.b.Freeze(independentOwner, independentProject, e.candidate.ID)
	independentRequire(t, err)
	_, frozenBefore, err := e.b.ReportHTML(independentOwner, independentProject, frozen.ID)
	independentRequire(t, err)
	var intact *intactCandidateSnapshot
	if e.scenario.ID == "IOR-054" {
		intact = e.createIntactSnapshot()
	}
	backup, err := e.b.Backup(independentOwner, independentProject, t.TempDir())
	independentRequire(t, err)

	staged, err := bolt.Open(backup.Path, 0o600, &bolt.Options{Timeout: time.Second})
	independentRequire(t, err)
	mutateErr := staged.Update(func(tx *bolt.Tx) error {
		return injectIndependentFault(tx, e.candidate, e.baseline, expect.StorageFault)
	})
	independentRequire(t, staged.Close())
	independentRequire(t, mutateErr)
	backupBytes, err := os.ReadFile(backup.Path)
	independentRequire(t, err)
	// Bind the detached copy's checksum to its actual bytes. A checksum-only
	// restore check would otherwise mask the missing semantic validation.
	mutatedChecksum := independentSHA(backupBytes)
	if mutatedChecksum == backup.SHA256 {
		t.Fatal("fault injection did not change the detached backup")
	}
	independentRequire(t, os.WriteFile(backup.Path+".sha256", []byte(mutatedChecksum+"\n"), 0o600))

	settings := board.Settings{ProjectID: independentProject, ProductID: "DEMO", Clock: func() time.Time { return e.now }}
	livePath := filepath.Join(t.TempDir(), "live-fault.db")
	independentRequire(t, os.WriteFile(livePath, backupBytes, 0o600))
	check := func(path, phase string) {
		t.Helper()
		app, err := board.Open(path, settings)
		independentRequire(t, err)
		defer app.Close()
		_, err = app.Assess(independentOwner, independentProject, e.candidate.ID)
		independentEqual(t, phase+" rejects new assessment", errors.Is(err, board.ErrIntegrity), true)
		_, err = app.Freeze(independentOwner, independentProject, e.candidate.ID)
		independentEqual(t, phase+" rejects new report", errors.Is(err, board.ErrIntegrity), true)
		_, historical, err := app.ReportHTML(independentOwner, independentProject, frozen.ID)
		independentRequire(t, err)
		independentEqual(t, phase+" retains immutable historical report", historical, frozenBefore)
		for name, receipt := range map[string]board.Receipt{"history": beforeHistory, "current": beforeCurrent} {
			original, err := app.SourceCSV(independentOwner, independentProject, receipt.ID)
			independentRequire(t, err)
			independentEqual(t, phase+" retains exact original bytes for "+name, original, e.raw(name))
		}
		if phase == "restored copy" {
			health, err := app.Health(independentOwner, independentProject)
			independentRequire(t, err)
			independentEqual(t, "restored integrity remains held", health.Hold, true)
			independentEqual(t, "reviewer cannot revalidate inconsistent evidence", errors.Is(app.Revalidate(independentOwner, independentProject), board.ErrIntegrity), true)
		}
		if intact != nil {
			e.checkOtherPairHeld(app, path, phase, *intact)
		}
	}
	check(livePath, "live detached copy")
	restorePath := filepath.Join(t.TempDir(), "restored-fault.db")
	restored, err := board.RestoreBackup(backup.Path, restorePath, settings)
	independentRequire(t, err)
	independentEqual(t, "content verification rejects recomputed copy checksum", restored.IntegrityOK, false)
	check(restorePath, "restored copy")
	t.Log("read-only warning in the surrounding browser view needs separate evidence")
}

type intactCandidateSnapshot struct {
	candidate board.Candidate
	receipt   board.Receipt
	preview   board.Preview
	report    board.Report
	html      []byte
}

func (e *independentEnvironment) createIntactSnapshot() *intactCandidateSnapshot {
	t := e.t
	manifest := independentManifest(t, e.scenario.Setup.SecondaryManifest.raw(t))
	candidate, _, err := e.b.CreateCandidate(independentAuthor, independentProject, board.CandidateInput{Manifest: manifest, BaselineDigest: e.baseline.Descriptor.Digest})
	independentRequire(t, err)
	preview := e.preview("intact", candidate.ID)
	receipt := e.confirm("intact")
	independentEqual(t, "other pair sufficient before storage fault", e.assess(candidate.ID, "").Status, "SUFFICIENT")
	report, err := e.b.Freeze(independentOwner, independentProject, candidate.ID)
	independentRequire(t, err)
	_, frozen, err := e.b.ReportHTML(independentOwner, independentProject, report.ID)
	independentRequire(t, err)
	return &intactCandidateSnapshot{candidate: candidate, receipt: receipt, preview: preview, report: report, html: frozen}
}

func (e *independentEnvironment) checkOtherPairHeld(app *board.Board, path, phase string, intact intactCandidateSnapshot) {
	t := e.t
	_, err := app.Assess(independentOwner, independentProject, intact.candidate.ID)
	independentEqual(t, phase+" global integrity blocks other candidate assessment", errors.Is(err, board.ErrIntegrity), true)
	_, err = app.Freeze(independentOwner, independentProject, intact.candidate.ID)
	independentEqual(t, phase+" global integrity blocks other candidate export", errors.Is(err, board.ErrIntegrity), true)
	_, retained, err := app.ReportHTML(independentOwner, independentProject, intact.report.ID)
	independentRequire(t, err)
	independentEqual(t, phase+" retains intact pair report bytes", retained, intact.html)
	before := detachedPhysicalState(t, path)
	replay, err := app.CommitImport(independentAuthor, independentProject, intact.preview.Token, e.raw("intact"))
	independentRequire(t, err)
	independentEqual(t, phase+" exact committed receipt remains available", replay, intact.receipt)
	independentEqual(t, phase+" exact receipt replay leaves durable transaction and bytes unchanged", detachedPhysicalState(t, path), before)
}

func injectIndependentFault(tx *bolt.Tx, candidate board.Candidate, baseline board.ApprovedBaseline, fault string) error {
	const history = "integrity-history"
	const current = "integrity-current"
	switch fault {
	case "candidateRunIndex":
		index := tx.Bucket([]byte("candidate_runs"))
		key := []byte(candidate.ID + "\x00" + history)
		if string(index.Get(key)) != history {
			return errors.New("detached fixture lacks historical candidate-run index")
		}
		return index.Delete(key)
	case "normalizedOutcome", "normalizedRows", "normalizedRunFinish":
		id := history
		if fault == "normalizedRunFinish" {
			id = current
		}
		runs := tx.Bucket([]byte("runs"))
		var run board.Run
		if err := json.Unmarshal(runs.Get([]byte(id)), &run); err != nil {
			return err
		}
		if fault == "normalizedRunFinish" {
			run.Closed = time.Date(2026, time.November, 17, 10, 45, 0, 0, time.UTC)
		} else {
			if len(run.Rows) != 2 || run.Rows[1].ResultID() != "H2" || run.Rows[1].Outcome() != "FAIL" {
				return errors.New("detached fixture lacks its designated failure row")
			}
			if fault == "normalizedOutcome" {
				run.Rows[1].Fields[16] = "PASS" // Literal outcome column in C02.
			} else {
				run.Rows = run.Rows[:1]
			}
		}
		return replaceDetachedJSON(runs, id, run)
	case "approvedRequiredFlag":
		approved := tx.Bucket([]byte("baselines"))
		digest := baseline.Descriptor.Digest
		var retained board.ApprovedBaseline
		if err := json.Unmarshal(approved.Get([]byte(digest)), &retained); err != nil {
			return err
		}
		if len(retained.Baseline.Behaviors) != 1 || !retained.Baseline.Behaviors[0].Required {
			return errors.New("detached fixture lacks its approved required behavior")
		}
		retained.Baseline.Behaviors[0].Required = false
		return replaceDetachedJSON(approved, digest, retained)
	default:
		return fmt.Errorf("unknown independent storage fault %q", fault)
	}
}

func replaceDetachedJSON(bucket *bolt.Bucket, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return bucket.Put([]byte(key), raw)
}

func (e *independentEnvironment) checkAbandonedPreviewRestart() {
	t := e.t
	committed := e.commit("a")
	abandoned := e.preview("b", e.candidate.ID)
	inspectDetachedStore(t, e.path, func(tx *bolt.Tx, _ []byte) {
		if tx.Bucket([]byte("previews")).Get([]byte(abandoned.Token)) == nil || tx.Bucket([]byte("preview_csv")).Stats().KeyN == 0 {
			t.Error("fixture did not persist its staged upload before restart")
		}
	})
	independentRequire(t, e.b.SuspendAssessments(independentOwner, independentProject))
	independentRequire(t, e.b.Close())
	e.now = independentTime(t, "2026-11-19T12:00:00Z")
	e.open()
	inspectDetachedStore(t, e.path, func(tx *bolt.Tx, _ []byte) {
		if tx.Bucket([]byte("previews")).Get([]byte(abandoned.Token)) != nil {
			t.Error("startup retained the expired abandoned preview binding")
		}
		independentEqual(t, "startup clears the abandoned staged upload bytes", tx.Bucket([]byte("preview_csv")).Stats().KeyN, 0)
	})
	raw, err := e.b.SourceCSV(independentOwner, independentProject, committed.ID)
	independentRequire(t, err)
	independentEqual(t, "cleanup preserves committed source bytes", raw, e.raw("a"))
	if _, err := e.b.CommitImport(independentAuthor, independentProject, abandoned.Token, e.raw("b")); err == nil {
		t.Error("expired abandoned preview committed after startup")
	}
	_, err = e.b.Assess(independentOwner, independentProject, e.candidate.ID)
	independentEqual(t, "startup cleanup preserves incident hold", errors.Is(err, board.ErrIntegrity), true)
}

func (e *independentEnvironment) checkBadBackupSidecar() {
	t := e.t
	e.commit("a")
	frozen, err := e.b.Freeze(independentOwner, independentProject, e.candidate.ID)
	independentRequire(t, err)
	_, archived, err := e.b.ReportHTML(independentOwner, independentProject, frozen.ID)
	independentRequire(t, err)
	backup, err := e.b.Backup(independentOwner, independentProject, t.TempDir())
	independentRequire(t, err)
	variant := independentDecode[struct{ Sidecar string }](t, e.scenario.Expected).Sidecar
	if variant == "missing" {
		independentRequire(t, os.Remove(backup.Path+".sha256"))
	} else {
		independentRequire(t, os.WriteFile(backup.Path+".sha256", []byte("unrecognized digest\n"), 0o600))
	}
	raw, err := os.ReadFile(backup.Path)
	independentRequire(t, err)
	independentEqual(t, "fault changes only the independent checksum metadata", independentSHA(raw), backup.SHA256)
	path := filepath.Join(t.TempDir(), "restored-sidecar.db")
	settings := board.Settings{ProjectID: independentProject, ProductID: "DEMO", Clock: func() time.Time { return e.now }}
	result, err := board.RestoreBackup(backup.Path, path, settings)
	independentRequire(t, err)
	independentEqual(t, "restore warns when the independent backup checksum is "+variant, result.IntegrityOK, false)
	restored, err := board.Open(path, settings)
	independentRequire(t, err)
	defer restored.Close()
	_, html, err := restored.ReportHTML(independentOwner, independentProject, frozen.ID)
	independentRequire(t, err)
	independentEqual(t, "read-only historical report bytes remain unchanged", html, archived)
	_, err = restored.Freeze(independentOwner, independentProject, e.candidate.ID)
	independentEqual(t, "unverified backup holds new export", errors.Is(err, board.ErrIntegrity), true)
}
