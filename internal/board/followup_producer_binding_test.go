package board_test

import (
	"bytes"
	"slices"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupProducerRecordingPrecedesSuccessorAndPreservesPriorPair(t *testing.T) {
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	app, _ := openBoard(t, func() time.Time { return now })
	prior := fixtureCandidate(t, app, vectorBaseline())
	now = now.Add(15 * time.Minute)
	producerAt := now
	captured := resultRow(prior, "T1", "1", "PRODUCER-ORIGINAL", "PRODUCER-RESULT", "2026-11-17T09:15:00Z", "2026-11-17T09:15:00Z", "PASS")
	if captured[5] != vectorManifestDigest || captured[6] != vectorBaselineDigest {
		t.Fatal("producer ticket did not capture the original independent descriptor pair")
	}
	now = now.Add(15 * time.Minute)
	nextRoster := vectorBaseline()
	nextRoster.ID = "CORRECTED-ROSTER"
	nextRoster.Behaviors[0].ID, nextRoster.Behaviors[0].Text = "BEH2", "Distinct harness expectations"
	nextRoster.Behaviors[0].Tests[0].ID, nextRoster.Behaviors[0].Policy.ID = "T2", "POL2"
	approved := approveFixture(t, app, nextRoster)
	corrected := vectorManifest()
	corrected.ReleaseID, corrected.EnvironmentID = "R2", "E2"
	successor, reused, err := app.CreateCandidate(supervisor, project, board.CandidateInput{
		Manifest: corrected, BaselineDigest: approved.Descriptor.Digest,
		PredecessorID: prior.ID, Correction: "New execution and contract inputs",
	})
	if err != nil || reused || successor.ManifestDigest == prior.ManifestDigest || successor.BaselineDigest == prior.BaselineDigest {
		t.Fatalf("corrected candidate did not establish a distinct pair: %v", err)
	}
	successorAt := now
	now = now.Add(30 * time.Minute)
	// Normalize the execution-time record after choosing the correction.
	// The producer inputs are retained rather than recovered from the UI.
	raw := csvBytes(t, captured)
	preview := previewFollowupFile(t, app, successor.ID, raw)
	if preview.OpenCandidateID != successor.ID || !slices.Equal(preview.HistoricalCandidates, []string{prior.ID}) {
		t.Fatal("old execution did not remain historical when previewed from the successor")
	}
	receipt, err := app.CommitImport(supervisor, project, preview.Token, raw)
	if err != nil || receipt.LedgerHighWater != 1 || !receipt.ImportedAt.Equal(now) ||
		receipt.OpenCandidateID != successor.ID || !slices.Equal(receipt.CandidateIDs, []string{prior.ID}) ||
		!slices.Equal(receipt.HistoricalCandidates, []string{prior.ID}) {
		t.Fatalf("committed historical receipt did not preserve producer attachment: %v", err)
	}
	original, err := app.SourceCSV(supervisor, project, receipt.ID)
	if err != nil || !bytes.Equal(original, raw) {
		t.Fatalf("application changed the normalized exact producer source: %v", err)
	}
	earlier := statusAssessment(t, app, prior.ID, "SUFFICIENT", 1)
	row := statusRun(t, earlier.Behaviors[0], "PRODUCER-ORIGINAL").Rows
	if len(row) != 1 || !slices.Equal(row[0].Fields[:], captured) || row[0].FinishedAt() != board.FormatUTC(producerAt) {
		t.Fatal("retained historical run has rewritten producer data")
	}
	later := statusAssessment(t, app, successor.ID, "BLOCKED", 1)
	if later.Behaviors[0].Status != "UNKNOWN" || later.Behaviors[0].CurrentRunID != "" {
		t.Fatal("prior producer result incorrectly supplies successor")
	}
	t.Logf("producer=%s successor=%s import=%s prior=%s current=BLOCKED prior_manifest=%s prior_baseline=%s exact_source=%s",
		board.FormatUTC(producerAt), board.FormatUTC(successorAt), board.FormatUTC(receipt.ImportedAt), earlier.Status,
		captured[5], captured[6], receipt.Hash)
	recordCorroboratedBoardObservation(t)
}
