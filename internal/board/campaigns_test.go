package board_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func FuzzResultsCSV(f *testing.F) {
	f.Add([]byte(resultsHeader))
	f.Add([]byte{})
	f.Add([]byte("{\x00}"))
	f.Add([]byte(resultsHeader + "broken,case,\"\n"))
	f.Add([]byte("\xef\xbb\xbf" + strings.ReplaceAll(resultsHeader, "\n", "\r\n")))
	f.Add([]byte(resultsHeader + fmt.Sprintf("reb-results/1.3,RESULT,RES1,DEMO,R1,%s,%s,BEH1,1,POL1,1,T1,RUN1,1,2026-11-17T12:00:00Z,2026-11-17T12:00:00Z,PASS,,\"example,\"\"quoted\"\"\ntext\"\n", vectorManifestDigest, vectorBaselineDigest)))
	f.Fuzz(func(t *testing.T, raw []byte) {
		before := sha256.Sum256(raw)
		rows, issues := board.ParseResultsCSV(raw)
		if sha256.Sum256(raw) != before {
			t.Fatal("parser changed original bytes")
		}
		if len(rows) > board.MaxCSVRows {
			t.Fatal("parser exceeded the row limit")
		}
		if len(issues) == 0 {
			for i, row := range rows {
				if row.LogicalRow != i+2 || row.PhysicalLine < row.LogicalRow {
					t.Fatalf("invalid accepted source row location: %#v", row)
				}
				if row.Kind() == board.RunOnly && (row.ResultID() != "" || row.Outcome() != "") {
					t.Fatal("accepted a result in a RUN_ONLY record")
				}
			}
		}
	})
}

func TestGeneratedDescriptorAndStatusProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(0x524542, 0x433031))
	domain := []rune{'a', ' ', '"', '\\', '\n', '\r', '\t', 0, '$', '<', '>', 'é', 'e', '\u0301', '😀', '\u2028'}
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		length := rng.IntN(50) + 1
		var text strings.Builder
		for j := 0; j < length; j++ {
			text.WriteRune(domain[rng.IntN(len(domain))])
		}
		baseline := vectorBaseline()
		baseline.Behaviors[0].Text = fmt.Sprintf("%04d:%s", i, text.String())
		copyPlan := baseline.Behaviors[0].Tests[0]
		copyPlan.ID = "T2"
		baseline.Behaviors[0].Tests = append(baseline.Behaviors[0].Tests, copyPlan)
		descriptor, err := baseline.Descriptor()
		if err != nil {
			t.Fatalf("seed index %d, valid baseline: %v", i, err)
		}
		slices.Reverse(baseline.Behaviors[0].Tests)
		reversed, err := baseline.Descriptor()
		if err != nil || reversed != descriptor {
			t.Fatalf("seed index %d, unordered producer roster changed canonical descriptor", i)
		}
		if seen[descriptor.Digest] || strings.HasSuffix(descriptor.Text, "\n") || strings.ContainsAny(descriptor.Text, "\n\r\t\x00") {
			t.Fatalf("seed index %d, descriptor reused identity or exposed control characters", i)
		}
		seen[descriptor.Digest] = true
		baseline.Behaviors[0].Text += string([]byte{0xed, 0xa0, 0x80})
		if _, err := baseline.Descriptor(); err == nil {
			t.Fatalf("seed index %d, descriptor accepted a UTF-8 surrogate", i)
		}
	}
	t.Log("seed 0x524542/0x433031: 1000 distinct valid Unicode rosters, 1000 shuffled valid rosters, 1000 invalid Unicode rosters")
}

type modeledRun struct {
	production board.Run
	variant    string
	tagged     bool
}

func TestGeneratedStatefulAssessmentAgreement(t *testing.T) {
	rng := rand.New(rand.NewPCG(0x524542, 0x443137))
	candidate := board.Candidate{ManifestDigest: vectorManifestDigest, BaselineDigest: vectorBaselineDigest}
	behavior := vectorBaseline().Behaviors[0]
	var checks uint64
	for i := 0; i < 1000; i++ {
		var history []modeledRun
		now := fixtureTime
		for step := 0; step < 50; step++ {
			switch rng.IntN(8) {
			case 0, 1, 2, 3, 4, 5:
				tags := []string{board.Pass, "ERROR", "SKIPPED", board.Unknown, "", board.Fail}
				tag := tags[rng.IntN(len(tags))]
				closed := fixtureTime.Add(time.Duration(rng.IntN(6)) * time.Minute)
				id := fmt.Sprintf("RUN-%d-%d", i, step)
				stamp := board.FormatUTC(closed)
				row := runOnly(candidate, id, stamp)
				if tag != "" {
					row = resultRow(candidate, "T1", "1", id, "RES-"+id, stamp, stamp, tag)
				}
				parsed, issues := board.ParseResultsCSV(csvBytes(t, row))
				if len(issues) != 0 || len(parsed) != 1 {
					t.Fatalf("generator produced invalid evidence: %v", issues)
				}
				history = append(history, modeledRun{production: board.Run{
					ID: id, Closed: closed, Sequence: uint64(step + 1), Rows: parsed, ImportedAt: fixtureTime.Add(5 * time.Minute),
				}, variant: tag, tagged: tag == board.Pass || tag == board.Fail})
			case 6:
				now = now.Add(12 * time.Hour)
			case 7:
				bad := resultRow(candidate, "T1", "1", "BROKEN", "BAD", board.FormatUTC(now), board.FormatUTC(now), "succeeded")
				if _, issues := board.ParseResultsCSV(csvBytes(t, bad)); len(issues) == 0 {
					t.Fatal("accepted malformed result instead of retaining previous state")
				}
			}
			compareGeneratedAssessment(t, i, step, history, behavior, now)
			checks++
		}
	}
	t.Logf("seed 0x524542/0x443137: 1000 sequences of 50 operations, %d compared states with generated admissions, malformed input, clock transitions and repeated ties", checks)
}

func compareGeneratedAssessment(t *testing.T, sequence, step int, history []modeledRun, behavior board.Behavior, now time.Time) {
	t.Helper()
	input := make([]board.Run, 0, len(history))
	var latest time.Time
	for _, run := range history {
		input = append(input, run.production)
		if run.production.Closed.After(latest) {
			latest = run.production.Closed
		}
	}
	// Derive the expected run by filtering to the latest instant, then
	// replacing only with an incomplete or equally complete later entry.
	var pick modeledRun
	currentSet := false
	stickyFailure := false
	for _, run := range history {
		stickyFailure = stickyFailure || run.variant == "FAIL"
		if run.production.Closed.Equal(latest) && (!currentSet || !run.tagged || pick.tagged) {
			pick = run
			currentSet = true
		}
	}
	want := "UNKNOWN"
	if stickyFailure {
		want = "FAIL"
	} else if pick.variant == "PASS" {
		want = "PASS"
		if now.After(latest.Add(24 * time.Hour)) {
			want = "STALE"
		}
	}
	decision, _ := board.EvaluateBehavior(behavior, input, now)
	if decision.Status != want || decision.CurrentRunID != pick.production.ID {
		t.Fatalf("sequence %d step %d got %s/%q want %s/%q", sequence, step, decision.Status, decision.CurrentRunID, want, pick.production.ID)
	}
}

func TestAC004CSVBounds(t *testing.T) {
	candidate := board.Candidate{ManifestDigest: vectorManifestDigest, BaselineDigest: vectorBaselineDigest}
	t.Run("row_bound", func(t *testing.T) {
		var rows [][]string
		at := board.FormatUTC(fixtureTime)
		for i := 0; i <= board.MaxCSVRows; i++ {
			rows = append(rows, runOnly(candidate, "RUN"+strconv.Itoa(i), at))
		}
		atLimit, issues := board.ParseResultsCSV(csvBytes(t, rows[:board.MaxCSVRows]...))
		if len(issues) != 0 || len(atLimit) != board.MaxCSVRows {
			t.Fatalf("exact row limit rejected: %d rows, %v", len(atLimit), issues)
		}
		app, _ := openBoard(t, func() time.Time { return fixtureTime })
		committed := fixtureCandidate(t, app, vectorBaseline())
		started := time.Now()
		receipt := importBytes(t, app, committed.ID, csvBytes(t, rows[:board.MaxCSVRows]...))
		if len(receipt.AddedRuns) != board.MaxCSVRows || receipt.LedgerHighWater != board.MaxCSVRows {
			t.Fatalf("row-limit commit = %d new runs, ledger %d", len(receipt.AddedRuns), receipt.LedgerHighWater)
		}
		t.Logf("exact 10,000 data rows preview and commit wall time: %s", time.Since(started))
		_, issues = board.ParseResultsCSV(csvBytes(t, rows...))
		if len(issues) != 1 || issues[0].Code != "TOO_MANY_ROWS" {
			t.Fatalf("limit plus one = %v", issues)
		}
	})
	t.Run("byte_bound", func(t *testing.T) {
		at := board.FormatUTC(fixtureTime)
		raw := []byte(resultsHeader)
		for i := 0; len(raw) < board.MaxCSVBytes; i++ {
			row := resultRow(candidate, "T1", "1", "RUN"+strconv.Itoa(i), "RES"+strconv.Itoa(i), at, at, board.Pass)
			unedited := bytes.TrimPrefix(csvBytes(t, row), []byte(resultsHeader))
			spare := board.MaxCSVBytes - len(raw) - len(unedited)
			if spare < 0 {
				t.Fatalf("bad capacity fixture, final row has %d spare bytes", spare)
			}
			padding := min(4000, spare)
			if remaining := spare - padding; remaining > 0 && remaining < 400 {
				padding -= 400
			}
			row[len(row)-1] = strings.Repeat("x", padding)
			raw = append(raw, bytes.TrimPrefix(csvBytes(t, row), []byte(resultsHeader))...)
		}
		if len(raw) != board.MaxCSVBytes {
			t.Fatal("capacity fixture did not reach exact byte limit")
		}
		if _, issues := board.ParseResultsCSV(raw); len(issues) != 0 {
			t.Fatalf("exact byte limit rejected: %v", issues)
		}
		app, _ := openBoard(t, func() time.Time { return fixtureTime })
		committed := fixtureCandidate(t, app, vectorBaseline())
		started := time.Now()
		receipt := importBytes(t, app, committed.ID, raw)
		original, err := app.SourceCSV(supervisor, project, receipt.ID)
		if err != nil || !bytes.Equal(original, raw) {
			t.Fatalf("max-size original not retained: %d bytes, %v", len(original), err)
		}
		t.Logf("exact 10 MiB preview, commit and readback wall time: %s", time.Since(started))
		if _, issues := board.ParseResultsCSV(append(raw, '\n')); len(issues) != 1 || issues[0].Code != "FILE_TOO_LARGE" {
			t.Fatalf("one byte above maximum = %v", issues)
		}
	})
}
