package board_test

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestTicket80MaterialRetryPrecedesMissingPredecessorWithoutInvalidatingPreview(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	candidate := fixtureCandidate(t, app, vectorBaseline())
	historical, err := app.Freeze(supervisor, project, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeMetadata, before, err := app.ReportHTML(supervisor, project, historical.ID)
	if err != nil {
		t.Fatal(err)
	}
	at := board.FormatUTC(fixtureTime)
	raw := csvBytes(t, resultRow(candidate, "T1", "1", "REUSE-RUN", "REUSE-RESULT", at, at, "PASS"))
	preview, err := app.PreviewImport(supervisor, project, candidate.ID, raw)
	if err != nil || len(preview.Issues) != 0 || preview.Token == "" {
		t.Fatalf("stage valid import: %#v %v", preview, err)
	}
	input := board.CandidateInput{Manifest: vectorManifest(), BaselineDigest: candidate.BaselineDigest,
		PredecessorID: "C-" + strings.Repeat("f", 64), DisplayVersion: "retry label", Correction: "retry note"}
	input.Manifest.ApplicationVersion = "1.0.0-relabelled"
	input.Manifest.ReleaseID = "RETRY-RELEASE"
	got, reused, err := app.CreateCandidate(supervisor, project, input)
	if err != nil || !reused || !reflect.DeepEqual(got, candidate) {
		t.Fatalf("same material retry must retain the original before predecessor resolution: %#v reused=%t %v", got, reused, err)
	}
	input.Manifest.BuildDigest = strings.Repeat("d", 64)
	if got, reused, err := app.CreateCandidate(supervisor, project, input); !errors.Is(err, board.ErrNotFound) || reused || got.ID != "" {
		t.Fatalf("new material must reject an unknown explicit predecessor: %#v reused=%t %v", got, reused, err)
	}
	if all, err := app.Candidates(supervisor, project); err != nil || len(all) != 1 || !reflect.DeepEqual(all[0], candidate) {
		t.Fatalf("retry or refusal changed retained candidates: %#v %v", all, err)
	}
	receipt, err := app.CommitImport(supervisor, project, preview.Token, nil)
	if err != nil || receipt.LedgerHighWater != 1 {
		t.Fatalf("retry/refusal invalidated the unchanged data revision and staged import: %#v %v", receipt, err)
	}
	assessment, err := app.Assess(supervisor, project, candidate.ID)
	if err != nil || assessment.Status != board.Sufficient || assessment.Counts[board.Pass] != 1 {
		t.Fatalf("committed evidence lost its assessment: %#v %v", assessment, err)
	}
	afterMetadata, after, err := app.ReportHTML(supervisor, project, historical.ID)
	if err != nil || !bytes.Equal(before, after) || !reflect.DeepEqual(beforeMetadata, afterMetadata) {
		t.Fatalf("retry/commit changed frozen historical report bytes: %v", err)
	}
}
