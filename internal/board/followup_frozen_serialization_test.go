package board_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

type serializationClock struct {
	barrierCall int32
	armed       atomic.Bool
	calls       atomic.Int32
	writing     chan struct{}
	after       chan struct{}
	release     chan struct{}
}

func (c *serializationClock) now() time.Time {
	if c.armed.Load() {
		switch c.calls.Add(1) {
		case c.barrierCall:
			close(c.writing)
			<-c.release
		case c.barrierCall + 1:
			close(c.after)
		}
	}
	return fixtureTime
}

// AC-010-03. Hold the first real writer at the existing clock seam and
// submit the competing operation before release. Independent literals
// describe each coherent snapshot at the resulting audit order.
func TestFollowupImportAndFreezeSerializeOneEvidenceSnapshot(t *testing.T) {
	for _, ordering := range []struct {
		name        string
		importFirst bool
		barrierCall int32
		wantStatus  string
		wantLedger  uint64
	}{
		{"import-before-snapshot", true, 2, "BLOCKED", 2},
		{"import-after-snapshot", false, 1, "SUFFICIENT", 1},
	} {
		t.Run(ordering.name, func(t *testing.T) {
			c := &serializationClock{barrierCall: ordering.barrierCall,
				writing: make(chan struct{}), after: make(chan struct{}), release: make(chan struct{})}
			app, _ := openBoard(t, c.now)
			candidate := fixtureCandidate(t, app, vectorBaseline())
			firstRow := resultRow(candidate, "T1", "1", "FROZEN-PASS", "PASS-RESULT", intakeEarlier, intakeEarlier, "PASS")
			originalCSV := csvBytes(t, firstRow)
			original := importBytes(t, app, candidate.ID, originalCSV)
			secondRow := resultRow(candidate, "T1", "1", "ARRIVING-FAIL", "FAIL-RESULT", intakeCurrent, intakeCurrent, "FAIL")
			arrivingCSV := csvBytes(t, secondRow)
			preview := previewFollowupFile(t, app, candidate.ID, arrivingCSV)
			oldAudit := intakeAudit(t, app)

			release := sync.OnceFunc(func() { close(c.release) })
			var workers sync.WaitGroup
			defer func() {
				release()
				workers.Wait()
			}()
			type imported struct {
				receipt board.Receipt
				err     error
			}
			type frozen struct {
				report board.Report
				err    error
			}
			importing, freezing := make(chan imported, 1), make(chan frozen, 1)
			beginImport := func(started chan<- struct{}) {
				workers.Go(func() {
					close(started)
					receipt, err := app.CommitImport(supervisor, project, preview.Token, arrivingCSV)
					importing <- imported{receipt, err}
				})
			}
			beginFreeze := func(started chan<- struct{}) {
				workers.Go(func() {
					close(started)
					report, err := app.Freeze(supervisor, project, candidate.ID)
					freezing <- frozen{report, err}
				})
			}
			c.armed.Store(true)
			first, second := make(chan struct{}), make(chan struct{})
			if ordering.importFirst {
				// Confirmation first reads its submitted time. The next
				// clock visit dates its material audit inside the write.
				beginImport(first)
				awaitSerializationSignal(t, c.writing, "import audit in writer")
				beginFreeze(second)
			} else {
				// Freeze consults the assessment clock in its writer.
				beginFreeze(first)
				awaitSerializationSignal(t, c.writing, "freeze clock in writer")
				beginImport(second)
				awaitSerializationSignal(t, c.after, "competing import pre-write clock")
			}
			awaitSerializationSignal(t, second, "competing public operation submitted")
			release()
			workers.Wait()
			committed, snapshot := <-importing, <-freezing
			if committed.err != nil || snapshot.err != nil {
				t.Fatalf("concurrent import or freeze failed: import=%v freeze=%v", committed.err, snapshot.err)
			}
			report := snapshot.report
			commit := committed.receipt
			if commit.LedgerHighWater != 2 || !slices.Equal(commit.AddedRuns, []string{"ARRIVING-FAIL"}) ||
				report.LedgerHighWater != ordering.wantLedger || report.Status != ordering.wantStatus || !report.AssessedAt.Equal(fixtureTime) {
				t.Fatalf("mixed or unexpected decision: imported ledger=%d report=%s ledger=%d at=%s", commit.LedgerHighWater, report.Status, report.LedgerHighWater, report.AssessedAt)
			}
			checkSerializationAudit(t, intakeAudit(t, app), len(oldAudit), commit.ID, report.ID, ordering.importFirst)
			_, html, err := app.ReportHTML(supervisor, project, report.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantRows := [][]string{firstRow, secondRow}[:ordering.wantLedger]
			checkSerializedHTML(t, html, report, candidate, wantRows)
			for _, original := range []struct {
				id  string
				raw []byte
			}{{original.ID, originalCSV}, {commit.ID, arrivingCSV}} {
				stored, err := app.SourceCSV(supervisor, project, original.id)
				if err != nil || !bytes.Equal(stored, original.raw) {
					t.Fatalf("serialized receipt lost its exact original: %v", err)
				}
			}
			live, err := app.Assess(supervisor, project, candidate.ID)
			if err != nil || live.Status != "BLOCKED" || live.LedgerHighWater != 2 ||
				len(live.Behaviors) != 1 || live.Behaviors[0].CurrentRunID != "ARRIVING-FAIL" {
				t.Fatalf("live board did not converge to retained failure: %s, %v", live.Status, err)
			}
			_, again, err := app.ReportHTML(supervisor, project, report.ID)
			if err != nil || !bytes.Equal(html, again) {
				t.Fatalf("later state changed the frozen decision bytes: %v", err)
			}
			t.Logf("AC-010-03 snapshot=%s ledger=%d coherent_rows=%d live=BLOCKED at=2", report.Status, report.LedgerHighWater, len(wantRows))
			recordCorroboratedBoardObservation(t)
		})
	}
}

func awaitSerializationSignal(t *testing.T, signal <-chan struct{}, event string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s was not observed", event)
	}
}

func checkSerializationAudit(t *testing.T, events []board.AuditEvent, prefix int, receiptID, reportID string, importFirst bool) {
	t.Helper()
	if len(events) != prefix+2 {
		t.Fatalf("concurrent writes appended %d audit events, want 2", len(events)-prefix)
	}
	commit, freeze := events[prefix], events[prefix+1]
	if !importFirst {
		freeze, commit = commit, freeze
	}
	if commit.Action != "import.commit" || commit.Outcome != "success" || commit.Actor != supervisor ||
		!slices.Contains(commit.References, receiptID) || freeze.Action != "report.freeze" || freeze.Outcome != "success" ||
		freeze.Actor != supervisor || !slices.Contains(freeze.References, reportID) {
		t.Fatalf("material audit did not establish the expected serialization: %s then %s", events[prefix].Action, events[prefix+1].Action)
	}
}

func checkSerializedHTML(t *testing.T, raw []byte, report board.Report, candidate board.Candidate, wantRows [][]string) {
	t.Helper()
	root, anchors := offlineParse(t, raw)
	offlineSafeLinks(t, root, anchors)
	descriptor := anchors["evidence-descriptor"]
	if descriptor == nil {
		t.Fatal("frozen report lacks the canonical evidence descriptor")
	}
	text := descriptor.plain()
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != report.SHA256 || fmt.Sprintf("%x", sha256.Sum256([]byte(text))) != report.EvidenceDigest {
		t.Fatal("report body and canonical decision input did not verify independently")
	}
	if !strings.Contains(root.plain(), "Historical assessment · "+report.Status) {
		t.Fatal("the visible historical status disagrees with the bound report")
	}
	var envelope []json.RawMessage
	if err := json.Unmarshal([]byte(text), &envelope); err != nil || len(envelope) != 7 {
		t.Fatalf("invalid canonical evidence envelope: %v", err)
	}
	manifest := independentDecode[string](t, envelope[1])
	baseline := independentDecode[string](t, envelope[2])
	ledger := independentDecode[uint64](t, envelope[4])
	stamp := independentDecode[string](t, envelope[5])
	if manifest != candidate.ManifestDigest || baseline != candidate.BaselineDigest ||
		ledger != report.LedgerHighWater || stamp != "2026-11-17T12:00:00Z" {
		t.Fatal("canonical descriptor uses a candidate, time or ledger outside the snapshot")
	}
	rows := independentDecode[[][]json.RawMessage](t, envelope[6])
	if len(rows) != len(wantRows) {
		t.Fatalf("frozen ledger=%d exported %d rows, want %d", ledger, len(rows), len(wantRows))
	}
	for i, row := range rows {
		if len(row) != 20 {
			t.Fatalf("frozen canonical source row %d does not match C03", i+1)
		}
		for field, expected := range wantRows[i] {
			if actual := independentDecode[string](t, row[field]); actual != expected {
				t.Fatalf("snapshot evidence row %d field %d = %q, want %q", i+1, field+1, actual, expected)
			}
		}
		sequence := independentDecode[uint64](t, row[19])
		run, result := wantRows[i][12], wantRows[i][2]
		if sequence != uint64(i+1) || anchors["runid-"+run] == nil || !strings.Contains(root.plain(), result) {
			t.Fatalf("missing or mixed visible evidence row %d: seq=%d run=%s result=%s", i+1, sequence, run, result)
		}
	}
}
