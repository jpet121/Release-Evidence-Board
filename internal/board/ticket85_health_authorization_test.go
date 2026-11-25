package board_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"release-evidence-board/internal/board"
)

func TestTicket85DeniedHealthReadDoesNotWriteDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health-access.db")
	app, err := board.Open(path, board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := app.Bootstrap("reviewer", "synthetic-health-access-password"); err != nil {
		t.Fatal(err)
	}
	// Observe the public operation's prohibited durable side effect, without
	// decoding database internals. No concurrent writer exists in this fixture.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Health("unassigned", "pilot"); !errors.Is(err, board.ErrForbidden) {
		t.Fatalf("unassigned health request did not deny access: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("denied health request committed a database write before authorization")
	}
	if health, err := app.Health("reviewer", "pilot"); err != nil || !health.DatabaseOK {
		t.Fatalf("authorized operator could not perform the initial health probe: %v %+v", err, health)
	}
}
