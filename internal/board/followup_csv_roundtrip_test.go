package board_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupCSVFlavorRetainsSourceAndParsedValues(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	candidate := fixtureCandidate(t, app, vectorBaseline())
	for i, check := range []struct {
		name    string
		excerpt string
		format  func([]byte) []byte
	}{
		{"bom", "literal Unicode 😀", func(raw []byte) []byte { return append([]byte{0xef, 0xbb, 0xbf}, raw...) }},
		{"lf-and-empty-optionals", "", nil},
		{"crlf", "CRLF record delimiter", func(raw []byte) []byte {
			return bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n"))
		}},
		{"quoted-comma", "alpha,beta", nil},
		{"escaped-quote", `value "inside" quote`, nil},
		{"embedded-newline", "first line\nsecond line", nil},
	} {
		t.Run(check.name, func(t *testing.T) {
			run := fmt.Sprintf("FLAVOR-%d", i+1)
			row := resultRow(candidate, "T1", "1", run, run+"-RESULT", intakeCurrent, intakeCurrent, "PASS")
			row[18] = check.excerpt
			raw := csvBytes(t, row)
			if check.format != nil {
				raw = check.format(raw)
			}
			receipt := importBytes(t, app, candidate.ID, raw)
			if receipt.LedgerHighWater != uint64(i+1) || receipt.Rows != 1 {
				t.Fatal("valid CSV dialect did not commit the exact row")
			}
			checkImportedCSVRow(t, app, candidate.ID, receipt.ID, raw, row)
			recordCorroboratedBoardObservation(t)
		})
	}
}

func TestFollowupCSVMaximumFieldValuesSurviveCommit(t *testing.T) {
	const targetProject = project
	id := func(char string) string { return strings.Repeat(char, 80) }
	product := id("P")
	app, err := board.Open(filepath.Join(t.TempDir(), "limit.db"), board.Settings{
		ProjectID: targetProject, ProductID: product, Clock: func() time.Time { return fixtureTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := app.Bootstrap(supervisor, testPassword); err != nil {
		t.Fatal(err)
	}
	roster := vectorBaseline()
	roster.ProductID, roster.ID = product, id("B")
	roster.Behaviors[0].ID, roster.Behaviors[0].Policy.ID, roster.Behaviors[0].Tests[0].ID = id("H"), id("O"), id("T")
	approved := approveFixture(t, app, roster)
	manifest := vectorManifest()
	manifest.ProductID, manifest.ReleaseID = product, id("R")
	candidate, reused, err := app.CreateCandidate(supervisor, project, board.CandidateInput{
		Manifest: manifest, BaselineDigest: approved.Descriptor.Digest,
	})
	if err != nil || reused {
		t.Fatalf("length-bound identities did not produce a valid approved candidate: %v", err)
	}
	row := resultRow(candidate, id("T"), "1", id("N"), id("S"), intakeCurrent, intakeCurrent, "PASS")
	row[3], row[4], row[7], row[9] = product, manifest.ReleaseID, id("H"), id("O")
	prefix := "https://example.invalid/"
	row[17], row[18] = prefix+strings.Repeat("q", 2048-len(prefix)), strings.Repeat("😀", 4000)
	raw := csvBytes(t, row)
	receipt := importBytes(t, app, candidate.ID, raw)
	checkImportedCSVRow(t, app, candidate.ID, receipt.ID, raw, row)
	recordCorroboratedBoardObservation(t)
}

func checkImportedCSVRow(t *testing.T, app *board.Board, candidateID, receiptID string, raw []byte, expected []string) {
	t.Helper()
	source, err := app.SourceCSV(supervisor, project, receiptID)
	if err != nil || !bytes.Equal(source, raw) {
		t.Fatalf("original protected bytes changed during normalization: %v", err)
	}
	state, err := app.Assess(supervisor, project, candidateID)
	if err != nil || state.Status != "SUFFICIENT" || len(state.Behaviors) != 1 {
		t.Fatalf("valid imported row did not supply a sufficient snapshot: %v", err)
	}
	run := statusRun(t, state.Behaviors[0], expected[12])
	if len(run.Rows) != 1 || !slices.Equal(run.Rows[0].Fields[:], expected) ||
		run.Rows[0].LogicalRow != 2 || run.Rows[0].PhysicalLine != 2 {
		t.Fatal("retained parsed row differs from the exact producer field values and coordinates")
	}
}

func TestFollowupCSVHeaderOnlyReorderAndGappedSlots(t *testing.T) {
	fx := newAtomicIntake(t)
	header := []byte(resultsHeader)
	emptyReceipt := importBytes(t, fx.app, fx.candidate.ID, header)
	if emptyReceipt.Rows != 0 || emptyReceipt.LedgerHighWater != 2 ||
		len(emptyReceipt.AddedRuns) != 0 || len(emptyReceipt.ExistingRuns) != 0 {
		t.Fatal("header-only upload invented evidence")
	}
	emptySource, err := fx.app.SourceCSV(supervisor, project, emptyReceipt.ID)
	if err != nil || !bytes.Equal(emptySource, header) {
		t.Fatalf("header-only no-op receipt lost its original: %v", err)
	}
	raw := csvBytes(t, fx.current[2], fx.current[0], fx.current[1])
	noOp := importBytes(t, fx.app, fx.candidate.ID, raw)
	if noOp.ID == fx.original.ID || noOp.Rows != 3 || noOp.LedgerHighWater != 2 ||
		len(noOp.AddedRuns) != 0 || !slices.Equal(noOp.ExistingRuns, []string{"CURRENT-PASS"}) ||
		!slices.Equal(noOp.PreviousReceipts, []string{fx.original.ID}) {
		t.Fatal("reordered equivalent source did not obtain a distinct no-op receipt")
	}
	retained, err := fx.app.SourceCSV(supervisor, project, noOp.ID)
	if err != nil || !bytes.Equal(retained, raw) {
		t.Fatalf("reorder no-op lost exact source bytes: %v", err)
	}
	const later = "2026-11-17T11:30:00Z"
	gapped := intakeThreeRows(fx.candidate, "GAPPED", later, "PASS")
	importBytes(t, fx.app, fx.candidate.ID, csvBytes(t, gapped[0], gapped[2]))
	state := statusAssessment(t, fx.app, fx.candidate.ID, "BLOCKED", 3)
	behavior := state.Behaviors[0]
	if behavior.Status != "UNKNOWN" || behavior.CurrentRunID != "GAPPED" || behavior.Passed != 2 ||
		len(behavior.Reasons) != 1 || behavior.Reasons[0].Code != "MISSING_SLOT" || behavior.Reasons[0].Attempt != 2 {
		t.Fatal("retained attempt indexes 1 and 3 changed or hid the missing slot 2")
	}
	recordCorroboratedBoardObservation(t)
}

func TestFollowupCSVWholeFileSyntaxAndCapacityRefusal(t *testing.T) {
	fx := newAtomicIntake(t)
	neighbor := resultRow(fx.candidate, "T1", "1", "NEIGHBOR", "NEIGHBOR-RESULT", intakeMiddle, intakeMiddle, "FAIL")
	good := string(csvBytes(t, neighbor))
	payload := strings.TrimPrefix(good, resultsHeader)
	for _, check := range []struct {
		name   string
		raw    string
		code   string
		row    int
		column string
	}{
		{"malformed-quote-after-valid", good + `"unterminated`, "CSV_SYNTAX", 3, "schema_version"},
		{"wrong-column-count-after-valid", good + "one,two\n", "COLUMN_COUNT", 3, ""},
		{"extra-header", strings.Replace(resultsHeader, "failure_excerpt", "failure_excerpt,unexpected", 1) + payload, "HEADER", 1, ""},
		{"missing-header", strings.Replace(resultsHeader, ",failure_excerpt", "", 1) + payload, "HEADER", 1, ""},
		{"reordered-header", strings.Replace(resultsHeader, "source_uri,failure_excerpt", "failure_excerpt,source_uri", 1) + payload, "HEADER", 1, "source_uri"},
		{"duplicate-header", strings.Replace(resultsHeader, "failure_excerpt", "source_uri", 1) + payload, "HEADER", 1, "failure_excerpt"},
	} {
		t.Run(check.name, func(t *testing.T) {
			fx.rejectRaw(t, []byte(check.raw), check.code, check.row, check.column)
			recordCorroboratedBoardObservation(t)
		})
	}
	t.Run("one-row-over", func(t *testing.T) {
		rows := make([][]string, 10001)
		for i := range rows {
			rows[i] = runOnly(fx.candidate, fmt.Sprintf("EXTRA-%05d", i), intakeCurrent)
		}
		raw := csvBytes(t, rows...)
		if len(raw) >= 10485760 {
			t.Fatal("row-limit test also exceeded the byte limit")
		}
		fx.rejectRaw(t, raw, "TOO_MANY_ROWS", 10002, "")
		recordCorroboratedBoardObservation(t)
	})
	t.Run("one-byte-over", func(t *testing.T) {
		rows := make([][]string, 2800)
		for i := range rows {
			id := fmt.Sprintf("PAD-%05d", i)
			rows[i] = resultRow(fx.candidate, "T1", "1", id, id+"-RESULT", intakeCurrent, intakeCurrent, "PASS")
		}
		remaining := 10485761 - len(csvBytes(t, rows...))
		for _, row := range rows {
			pad := min(3900, remaining)
			row[18] = strings.Repeat("x", pad)
			remaining -= pad
		}
		raw := csvBytes(t, rows...)
		if remaining != 0 || len(raw) != 10485761 {
			t.Fatal("fixture did not exceed exactly one byte")
		}
		fx.rejectRaw(t, raw, "FILE_TOO_LARGE", 0, "")
		recordCorroboratedBoardObservation(t)
	})
}

func (fx atomicIntake) rejectRaw(t *testing.T, raw []byte, code string, line int, column string) {
	t.Helper()
	preview, err := fx.app.PreviewImport(supervisor, project, fx.candidate.ID, raw)
	if err != nil || preview.Token != "" || !slices.ContainsFunc(preview.Issues, func(issue board.Diagnostic) bool {
		return issue.Code == code && issue.LogicalRow == line && issue.PhysicalLine == line && issue.Column == column
	}) {
		t.Fatalf("wrong whole-file rejection %s at %d %q: token=%t diagnostics=%+v error=%v", code, line, column, preview.Token != "", preview.Issues, err)
	}
	if _, err := fx.app.Receipt(supervisor, project, fmt.Sprintf("I-%x", sha256.Sum256(raw))); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("rejected syntax or capacity file produced a committed receipt: %v", err)
	}
	assessment := statusAssessment(t, fx.app, fx.candidate.ID, "SUFFICIENT", 2)
	if len(assessment.Behaviors[0].Runs) != 2 || assessment.Behaviors[0].CurrentRunID != "CURRENT-PASS" {
		t.Fatal("valid prefix from rejected upload changed retained current evidence")
	}
	currentSource, err := fx.app.SourceCSV(supervisor, project, fx.original.ID)
	if err != nil || !bytes.Equal(currentSource, fx.initialCSV) {
		t.Fatalf("later rejected upload changed protected original: %v", err)
	}
}
