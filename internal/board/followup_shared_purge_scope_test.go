package board_test

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupPurgeProtectsExpiredSharerOutsideScope(t *testing.T) {
	now := fixtureTime
	clock := func() time.Time { return now }
	app, path := openBoard(t, clock)
	first := fixtureCandidate(t, app, vectorBaseline())
	now = now.Add(24 * time.Hour)
	second := newCustodySuccessor(t, app, first, "a")
	stamp := board.FormatUTC(now)
	raw := csvBytes(t,
		resultRow(first, "T1", "1", "SHARED-A", "SHARED-RESULT-A", stamp, stamp, "PASS"),
		resultRow(second, "T1", "1", "SHARED-B", "SHARED-RESULT-B", stamp, stamp, "PASS"))
	receipt := importBytes(t, app, first.ID, raw)
	if !slices.Equal(receipt.CandidateIDs, slices.Sorted(slices.Values([]string{first.ID, second.ID}))) {
		t.Fatalf("fixture did not commit one receipt for both candidates: %v", receipt.CandidateIDs)
	}

	type snapshot struct {
		report board.Report
		html   []byte
	}
	freeze := func(candidate board.Candidate) snapshot {
		t.Helper()
		frozen, err := app.Freeze(supervisor, project, candidate.ID)
		if err != nil || frozen.Status != board.Sufficient {
			t.Fatalf("freeze sufficient candidate %s: status=%s err=%v", candidate.ID, frozen.Status, err)
		}
		report, html, err := app.ReportHTML(supervisor, project, frozen.ID)
		if err != nil || len(html) == 0 {
			t.Fatalf("read frozen report %s: %v", frozen.ID, err)
		}
		return snapshot{report, html}
	}
	firstReport := freeze(first)
	secondReport := freeze(second)

	// A is older than the cutoff and B is exactly at the cutoff.
	now = now.Add(180 * 24 * time.Hour)
	keeper := newCustodySuccessor(t, app, first, "b")
	stamp = board.FormatUTC(now)
	keeperRaw := csvBytes(t, resultRow(keeper, "T1", "1", "KEEP-RUN", "KEEP-RESULT", stamp, stamp, "PASS"))
	keeperReceipt := importBytes(t, app, keeper.ID, keeperRaw)
	keeperReport := freeze(keeper)
	before, err := app.Candidates(supervisor, project)
	if err != nil || len(before) != 3 {
		t.Fatalf("read the three fixture candidates: count=%d err=%v", len(before), err)
	}

	partial := []string{first.ID}
	plan, err := app.PreviewPurge(supervisor, project, partial)
	if err != nil || len(plan.Ineligible) != 0 || plan.Confirmation == "" {
		t.Fatalf("preview the eligible first candidate: %#v, %v", plan, err)
	}
	if !slices.Equal(plan.SharedBlockers, []string{second.ID}) {
		t.Errorf("preview omitted expired unselected shared blocker %s: got %v", second.ID, plan.SharedBlockers)
	}
	if _, err := app.Purge(supervisor, project, partial, plan.Confirmation); !errors.Is(err, board.ErrInvalid) {
		t.Fatalf("partial purge of expired shared evidence: got %v, want ErrInvalid", err)
	}

	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	app, err = board.Open(path, board.Settings{ProjectID: project, ProductID: "DEMO", Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	after, err := app.Candidates(supervisor, project)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("denied purge changed the reopened candidate catalog: %v", err)
	}
	assertReceipt := func(want board.Receipt, original []byte) {
		t.Helper()
		saved, err := app.Receipt(supervisor, project, want.ID)
		if err != nil || !reflect.DeepEqual(saved, want) {
			t.Fatalf("receipt %s changed: %v", want.ID, err)
		}
		csv, err := app.SourceCSV(supervisor, project, want.ID)
		if err != nil || !bytes.Equal(csv, original) {
			t.Fatalf("original CSV for %s changed: %v", want.ID, err)
		}
	}
	assertReport := func(want snapshot) {
		t.Helper()
		saved, html, err := app.ReportHTML(supervisor, project, want.report.ID)
		if err != nil || !reflect.DeepEqual(saved, want.report) || !bytes.Equal(html, want.html) {
			t.Fatalf("frozen report %s changed: %v", want.report.ID, err)
		}
	}
	assertReceipt(receipt, raw)
	assertReport(firstReport)
	assertReport(secondReport)

	both := []string{first.ID, second.ID}
	whole, err := app.PreviewPurge(supervisor, project, both)
	if err != nil || len(whole.Ineligible) != 0 || len(whole.SharedBlockers) != 0 ||
		!slices.Equal(whole.ReceiptIDs, []string{receipt.ID}) ||
		!slices.Equal(whole.ReportIDs, []string{firstReport.report.ID, secondReport.report.ID}) {
		t.Fatalf("joint purge did not project just the eligible sharers' evidence: %#v, %v", whole, err)
	}
	if _, err := app.Purge(supervisor, project, both, whole.Confirmation); err != nil {
		t.Fatalf("confirming both eligible sharers failed: %v", err)
	}
	left, err := app.Candidates(supervisor, project)
	if err != nil || !reflect.DeepEqual(left, []board.Candidate{keeper}) {
		t.Fatalf("joint purge did not retain exactly the unselected unrelated candidate: %v", err)
	}
	if _, err := app.Receipt(supervisor, project, receipt.ID); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("joint purge retained its shared receipt: %v", err)
	}
	if _, err := app.SourceCSV(supervisor, project, receipt.ID); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("joint purge retained its shared CSV: %v", err)
	}
	for _, removed := range []snapshot{firstReport, secondReport} {
		if _, _, err := app.ReportHTML(supervisor, project, removed.report.ID); !errors.Is(err, board.ErrNotFound) {
			t.Fatalf("joint purge retained selected report %s: %v", removed.report.ID, err)
		}
	}
	assertReceipt(keeperReceipt, keeperRaw)
	assertReport(keeperReport)
}
