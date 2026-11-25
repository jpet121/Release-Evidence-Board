package board_test

import (
	"bytes"
	"encoding/csv"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

var fixtureTime = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

const resultsHeader = "schema_version,record_type,result_id,product_id,release_id,manifest_digest,baseline_digest,behavior_id,behavior_revision,policy_id,policy_revision,test_id,run_id,attempt_index,finished_at,run_finished_at,outcome,source_uri,failure_excerpt\n"

func fixtureCandidate(t *testing.T, app *board.Board, baseline board.Baseline) board.Candidate {
	t.Helper()
	approved := approveFixture(t, app, baseline)
	input := board.CandidateInput{Manifest: vectorManifest(), BaselineDigest: approved.Descriptor.Digest}
	candidate, reused, err := app.CreateCandidate(supervisor, project, input)
	if err != nil || reused {
		t.Fatalf("create fixture candidate: reused %v, error %v", reused, err)
	}
	return candidate
}

func TestAC00503SimultaneousExactRetriesAreReadOnly(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	c := fixtureCandidate(t, app, vectorBaseline())
	at := board.FormatUTC(fixtureTime)
	raw := csvBytes(t, resultRow(c, "T1", "1", "RUN1", "RES1", at, at, "PASS"))
	preview, err := app.PreviewImport(supervisor, project, c.ID, raw)
	if err != nil || len(preview.Issues) != 0 || preview.Token == "" {
		t.Fatalf("preview = %#v %v", preview, err)
	}
	receipt, err := app.CommitImport(supervisor, project, preview.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var running sync.WaitGroup
	for i := 0; i < 2; i++ {
		running.Go(func() {
			<-start
			replayed, err := app.CommitImport(supervisor, project, preview.Token, raw)
			if err == nil && (replayed.ID != receipt.ID || replayed.LedgerHighWater != 1) {
				err = errors.New("wrong replay receipt")
			}
			results <- err
		})
	}
	close(start)
	running.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	audit, err := app.Audit(supervisor, project)
	if err != nil {
		t.Fatal(err)
	}
	var imports int
	for _, event := range audit {
		if event.Action == "import.commit" {
			imports++
		}
	}
	if imports != 1 {
		t.Fatalf("found %d import commit audit events; want one", imports)
	}
}

func resultRow(c board.Candidate, test, attempt, run, result, finishedAt, runFinishedAt, outcome string) []string {
	return []string{
		"reb-results/1.3", "RESULT", result, "DEMO", "R1", c.ManifestDigest, c.BaselineDigest, "BEH1", "1",
		"POL1", "1", test, run, attempt, finishedAt, runFinishedAt, outcome, "", "",
	}
}

func runOnly(c board.Candidate, run, finishedAt string) []string {
	return []string{
		"reb-results/1.3", "RUN_ONLY", "", "DEMO", "R1", c.ManifestDigest, c.BaselineDigest, "BEH1", "1",
		"POL1", "1", "", run, "", "", finishedAt, "", "", "",
	}
}

func csvBytes(t *testing.T, rows ...[]string) []byte {
	t.Helper()
	var data bytes.Buffer
	data.WriteString(resultsHeader)
	w := csv.NewWriter(&data)
	if err := w.WriteAll(rows); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func importBytes(t *testing.T, app *board.Board, candidateID string, data []byte) board.Receipt {
	t.Helper()
	preview, err := app.PreviewImport(supervisor, project, candidateID, data)
	if err != nil || len(preview.Issues) != 0 || preview.Token == "" {
		t.Fatalf("preview = %#v, %v", preview, err)
	}
	receipt, err := app.CommitImport(supervisor, project, preview.Token, data)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestAC00513CommittedRetryPrecedesStalePreview(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	c := fixtureCandidate(t, app, vectorBaseline())
	at := "2026-11-17T12:00:00Z"
	raw := csvBytes(t, resultRow(c, "T1", "1", "RUN1", "RES1", at, at, "PASS"))
	preview, err := app.PreviewImport(supervisor, project, c.ID, raw)
	if err != nil || len(preview.Issues) != 0 {
		t.Fatalf("preview: %#v %v", preview, err)
	}
	receipt, err := app.CommitImport(supervisor, project, preview.Token, raw)
	if err != nil || !slices.Equal(receipt.AddedRuns, []string{"RUN1"}) || receipt.LedgerHighWater != 1 {
		t.Fatalf("first commit = %#v, %v", receipt, err)
	}
	next := csvBytes(t, runOnly(c, "RUN2", at))
	second := importBytes(t, app, c.ID, next)
	if second.LedgerHighWater != 2 {
		t.Fatalf("later run = %#v", second)
	}
	audit, err := app.Audit(supervisor, project)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := app.CommitImport(supervisor, project, preview.Token, raw)
	if err != nil || replayed.ID != receipt.ID || replayed.LedgerHighWater != 1 {
		t.Fatalf("stale exact retry = %#v, %v", replayed, err)
	}
	after, err := app.Audit(supervisor, project)
	if err != nil || len(after) != len(audit) {
		t.Fatalf("exact retry mutated audit: %d to %d, %v", len(audit), len(after), err)
	}
	if _, err := app.CommitImport(supervisor, project, preview.Token, next); !errors.Is(err, board.ErrConflict) {
		t.Fatalf("modified bytes using old token = %v, want conflict", err)
	}
}
