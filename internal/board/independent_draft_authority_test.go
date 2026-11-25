package board_test

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"

	bolt "go.etcd.io/bbolt"
	"release-evidence-board/internal/board"
)

const independentReducingReviewer = "reviewer-a"
const independentOriginReviewer = "reviewer-c"

func (e *independentEnvironment) checkDraftAuthority() {
	t := e.t
	area := independentDecode[struct{ Area string }](t, e.scenario.Expected).Area
	history := e.commit("history")
	oldReport, err := e.b.Freeze(independentOwner, independentProject, e.candidate.ID)
	independentRequire(t, err)
	independentEqual(t, "old candidate freezes its original failure", oldReport.Status, "BLOCKED")
	e.addAccount(independentReducingReviewer, board.Reviewer)
	e.addAccount(independentOriginReviewer, board.Reviewer)
	contents := independentBaseline(t, e.scenario.Setup.ApprovedBaseline.raw(t))
	draft, err := e.b.CreateDraft(independentOriginReviewer, independentProject, contents, board.DraftOptions{FromBaseline: e.baseline.Descriptor.Digest})
	independentRequire(t, err)
	independentEqual(t, "original draft author", draft.Author, independentOriginReviewer)
	contents = draft.Baseline
	contents.Behaviors[0].Policy.MinPassBasisPoints = 5000
	lowered, err := e.b.SaveDraft(independentReducingReviewer, independentProject, draft.ID, draft.Revision, contents)
	independentRequire(t, err)
	_, err = e.b.Approve(independentReducingReviewer, independentProject, lowered.ID, lowered.Revision)
	independentEqual(t, "intact D20 gate denies the reducing editor", errors.Is(err, board.ErrForbidden), true)
	if area == "intact independent review" {
		approved, err := e.b.Approve(independentOriginReviewer, independentProject, lowered.ID, lowered.Revision)
		independentRequire(t, err)
		independentEqual(t, "original high-mark author is independent of the later reduction", approved.Approver, independentOriginReviewer)
		wanted := board.ThresholdChange{CandidateID: e.candidate.ID, BehaviorID: "BEH1", Before: 10000, After: 5000,
			ReducingActors: []string{independentReducingReviewer}}
		independentEqual(t, "independent approval records exact affected threshold and reducing reviewer", approved.Thresholds, []board.ThresholdChange{wanted})
		report := e.freezeHalfPassingSuccessor(e.b, independentOriginReviewer, approved)
		independentEqual(t, "independently approved policy accepts one of two passing attempts", report.Status, "SUFFICIENT")
		independentEqual(t, "approval cannot alter the old candidate's failure", e.assess(e.candidate.ID, "").Status, "BLOCKED")
		e.checkAuthorityReportAndCounts(e.b, e.path, oldReport, 2)
		return
	}

	removeActor := func(tx *bolt.Tx) error { return removeOnlyReducingEditor(tx, lowered.ID) }
	app, path := e.b, e.path
	if area == "restored backup" {
		backup, _ := e.faultedBackup(removeActor)
		path = filepath.Join(t.TempDir(), "draft-authority-restore.db")
		restored, err := board.RestoreBackup(backup, path, e.settings())
		independentRequire(t, err)
		independentEqual(t, "transport-valid backup with removed actor is not safe", restored.IntegrityOK, false)
		app, err = board.Open(path, e.settings())
		independentRequire(t, err)
		defer app.Close()
	} else {
		independentRequire(t, board.MutateIndependentFixtureStore(app, removeActor))
	}
	retained := []retainedFixtureKey{{"drafts", lowered.ID}, {"candidates", e.candidate.ID}, {"baselines", e.baseline.Descriptor.Digest},
		{"receipts", history.Hash}, {"csv_bytes", history.Hash}, {"reports", oldReport.ID}, {"report_html", oldReport.ID}}
	stateBefore := e.retainedFingerprint(path, retained)
	independentEqual(t, "all old source and report values remain physically present at fault boundary", len(stateBefore), len(retained))
	if area == "restored backup" {
		err = app.Revalidate(independentOriginReviewer, independentProject)
		independentEqual(t, "restored draft fails reviewer revalidation", errors.Is(err, board.ErrIntegrity), true)
	}
	approved, err := app.Approve(independentReducingReviewer, independentProject, lowered.ID, lowered.Revision)
	independentEqual(t, "damaged draft cannot grant the reducing editor approval", errors.Is(err, board.ErrIntegrity), true)
	if err == nil {
		// Reproduce the old release-decision error only in this synthetic clone.
		published := e.freezeHalfPassingSuccessor(app, independentReducingReviewer, approved)
		t.Errorf("removed reducing editor self-approved and froze successor status %s", published.Status)
		return
	}
	if area == "live store" {
		err = app.Revalidate(independentOriginReviewer, independentProject)
		independentEqual(t, "live draft stays held through subsequent reviewer revalidation", errors.Is(err, board.ErrIntegrity), true)
	}
	health, err := app.Health(independentOwner, independentProject)
	independentRequire(t, err)
	independentEqual(t, "changed draft establishes integrity hold", health.Hold, true)
	_, _, err = app.CreateCandidate(independentAuthor, independentProject, board.CandidateInput{
		Manifest: independentManifest(t, e.scenario.Setup.SecondaryManifest.raw(t)), BaselineDigest: e.baseline.Descriptor.Digest,
		PredecessorID: e.candidate.ID, Correction: "Controlled separate execution revision"})
	independentEqual(t, "hold rejects even an otherwise valid new candidate", errors.Is(err, board.ErrIntegrity), true)
	_, err = app.Freeze(independentReducingReviewer, independentProject, e.candidate.ID)
	independentEqual(t, "hold prevents new freeze by the reducing reviewer", errors.Is(err, board.ErrIntegrity), true)
	independentEqual(t, "failed operations preserve protected records rather than resealing the edited draft", e.retainedFingerprint(path, retained), stateBefore)
	e.checkAuthorityReportAndCounts(app, path, oldReport, 1)
}

func removeOnlyReducingEditor(tx *bolt.Tx, id string) error {
	bucket := tx.Bucket([]byte("drafts"))
	key := []byte(id)
	original := bytes.Clone(bucket.Get(key))
	var draft board.Draft
	if err := json.Unmarshal(original, &draft); err != nil {
		return err
	}
	oldField, err := json.Marshal(draft.ThresholdEdits)
	if err != nil {
		return err
	}
	var remaining []board.ThresholdEdit
	removed := 0
	for _, edit := range draft.ThresholdEdits {
		if edit.Actor == independentReducingReviewer {
			if edit.BehaviorID != "BEH1" || edit.Before != 10000 || edit.After != 5000 {
				return errors.New("unexpected reducing actor data in the fixture")
			}
			removed++
		} else {
			remaining = append(remaining, edit)
		}
	}
	if removed != 1 {
		return fmt.Errorf("wanted exactly one reducing-editor entry, found %d", removed)
	}
	newField, err := json.Marshal(remaining)
	if err != nil {
		return err
	}
	prefix := []byte(`"ThresholdEdits":`)
	old := append(bytes.Clone(prefix), oldField...)
	if bytes.Count(original, old) != 1 {
		return errors.New("threshold-actor field could not be uniquely located")
	}
	// Lexically replace only the one persisted field under test. This avoids
	// changing unrelated field serialization or any protection companion.
	return bucket.Put(key, bytes.Replace(original, old, append(prefix, newField...), 1))
}

func (e *independentEnvironment) freezeHalfPassingSuccessor(app *board.Board, reviewer string, approval board.ApprovedBaseline) board.Report {
	t := e.t
	t.Helper()
	successor, reused, err := app.CreateCandidate(reviewer, independentProject, board.CandidateInput{
		Manifest: e.manifest, BaselineDigest: approval.Descriptor.Digest, PredecessorID: e.candidate.ID, Correction: "Approved 5000-basis-point mark"})
	independentRequire(t, err)
	independentEqual(t, "approved contract change yields distinct successor", reused, false)
	behavior := approval.Baseline.Behaviors[0]
	rows, err := csv.NewReader(bytes.NewReader(e.raw("halfPassingTemplate"))).ReadAll()
	independentRequire(t, err)
	for _, source := range rows[1:] {
		// Bind literal PASS/FAIL values to the new approved descriptor and
		// its actually assigned revisions. No outcome is calculated here.
		source[6] = approval.Descriptor.Digest
		source[8] = strconv.Itoa(behavior.Revision)
		source[10] = strconv.Itoa(behavior.Policy.Revision)
	}
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	independentRequire(t, writer.WriteAll(rows))
	raw := buffer.Bytes()
	t.Logf("new exact CSV SHA-256 %s", independentSHA(raw))
	preview, err := app.PreviewImport(independentAuthor, independentProject, successor.ID, raw)
	independentRequire(t, err)
	if len(preview.Issues) != 0 {
		t.Fatalf("literal half-passing evidence unexpectedly rejected: %v", preview.Issues)
	}
	_, err = app.CommitImport(independentAuthor, independentProject, preview.Token, raw)
	independentRequire(t, err)
	assessment, err := app.Assess(reviewer, independentProject, successor.ID)
	independentRequire(t, err)
	result := independentBehavior(t, assessment, "BEH1")
	independentEqual(t, "literal passing attempt count", result.Passed, 1)
	independentEqual(t, "literal planned attempt count", result.Planned, 2)
	independentEqual(t, "exact approval uses 5000 basis points", result.Policy.MinPassBasisPoints, 5000)
	report, err := app.Freeze(reviewer, independentProject, successor.ID)
	independentRequire(t, err)
	return report
}

func (e *independentEnvironment) checkAuthorityReportAndCounts(app *board.Board, path string, oldReport board.Report, want int) {
	t := e.t
	t.Helper()
	_, original, err := app.ReportHTML(independentOwner, independentProject, oldReport.ID)
	independentRequire(t, err)
	independentEqual(t, "original historical report bytes unchanged", independentSHA(original), oldReport.SHA256)
	inspectDetachedStore(t, path, func(tx *bolt.Tx, _ []byte) {
		for _, bucket := range []string{"baselines", "candidates", "reports"} {
			independentEqual(t, "physical count of "+bucket, tx.Bucket([]byte(bucket)).Stats().KeyN, want)
		}
	})
}
