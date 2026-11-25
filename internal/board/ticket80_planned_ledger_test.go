package board_test

import (
	"bytes"
	"errors"
	bolt "go.etcd.io/bbolt"
	"math"
	"reflect"
	"release-evidence-board/internal/board"
	"slices"
	"testing"
	"time"
)

func ticket80WriteMeta(t *testing.T, path string, values map[string]string) {
	t.Helper()
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	fault := db.Update(func(tx *bolt.Tx) error {
		for key, value := range values {
			if err := tx.Bucket([]byte("meta")).Put([]byte(key), []byte(value)); err != nil {
				return err
			}
		}
		return nil
	})
	if err := errors.Join(fault, db.Close()); err != nil {
		t.Fatal(err)
	}
}

func ticket80Reopen(t *testing.T, path string) *board.Board {
	t.Helper()
	app, err := board.Open(path, board.Settings{ProjectID: project, ProductID: "DEMO", Clock: func() time.Time { return fixtureTime }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	return app
}

func TestTicket80PlannedMultiRunLedgerAndExistingOnlyReceiptAtCapacity(t *testing.T) {
	app, path := openBoard(t, func() time.Time { return fixtureTime })
	candidate := fixtureCandidate(t, app, vectorBaseline())
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	ticket80WriteMeta(t, path, map[string]string{"ledger": "18446744073709551613"})
	app = ticket80Reopen(t, path)
	stamp := "2026-11-17T11:00:00Z"
	raw := csvBytes(t, resultRow(candidate, "T1", "1", "Z-LAST", "RESULT-Z", stamp, stamp, "PASS"), resultRow(candidate, "T1", "1", "A-FIRST", "RESULT-A", stamp, stamp, "PASS"))
	receipt := importBytes(t, app, candidate.ID, raw)
	if receipt.LedgerHighWater != math.MaxUint64 || !slices.Equal(receipt.AddedRuns, []string{"A-FIRST", "Z-LAST"}) {
		t.Fatalf("lexical multi-run plan was not persisted at capacity: %+v", receipt)
	}
	assessment, err := app.Assess(supervisor, project, candidate.ID)
	if err != nil || assessment.Status != board.Sufficient || assessment.LedgerHighWater != math.MaxUint64 || len(assessment.Behaviors) != 1 || assessment.Behaviors[0].CurrentRunID != "Z-LAST" {
		t.Fatalf("wrong tied-run decision at terminal ledger: %+v %v", assessment, err)
	}
	positions := map[string]uint64{}
	for _, run := range assessment.Behaviors[0].Runs {
		positions[run.ID] = run.Sequence
	}
	if !reflect.DeepEqual(positions, map[string]uint64{"A-FIRST": math.MaxUint64 - 1, "Z-LAST": math.MaxUint64}) {
		t.Fatalf("checked run sequences changed: %v", positions)
	}
	frozen, err := app.Freeze(supervisor, project, candidate.ID)
	if err != nil || frozen.LedgerHighWater != math.MaxUint64 {
		t.Fatal("terminal assessment did not freeze", err)
	}
	_, priorHTML, err := app.ReportHTML(supervisor, project, frozen.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	const padded = "00018446744073709551615"
	ticket80WriteMeta(t, path, map[string]string{"ledger": padded})
	app = ticket80Reopen(t, path)
	alternate := bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n"))
	noOp := importBytes(t, app, candidate.ID, alternate)
	if noOp.ID == receipt.ID || noOp.LedgerHighWater != math.MaxUint64 || len(noOp.AddedRuns) != 0 || !slices.Equal(noOp.ExistingRuns, receipt.AddedRuns) || !slices.Equal(noOp.PreviousReceipts, []string{receipt.ID}) {
		t.Fatalf("different-byte existing-only receipt changed ledger/evidence: %+v", noOp)
	}
	for id, want := range map[string][]byte{receipt.ID: raw, noOp.ID: alternate} {
		if src, err := app.SourceCSV(supervisor, project, id); err != nil || !bytes.Equal(src, want) {
			t.Fatal("original source changed", err)
		}
	}
	retained, html, err := app.ReportHTML(supervisor, project, frozen.ID)
	if err != nil || !reflect.DeepEqual(retained, frozen) || !bytes.Equal(html, priorHTML) {
		t.Fatal("no-addition receipt changed frozen report", err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	read := db.View(func(tx *bolt.Tx) error {
		if string(tx.Bucket([]byte("meta")).Get([]byte("ledger"))) != padded {
			t.Error("existing-only receipt rewrote ledger scalar")
		}
		return nil
	})
	if err := errors.Join(read, db.Close()); err != nil {
		t.Fatal(err)
	}
}
