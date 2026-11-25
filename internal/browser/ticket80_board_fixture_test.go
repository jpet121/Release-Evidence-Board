package browser

import (
	"path/filepath"
	"testing"

	"release-evidence-board/internal/board"
)

func newSessionBoard(t *testing.T) *board.Board {
	t.Helper()
	app, err := board.Open(filepath.Join(t.TempDir(), "session-board.db"), board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := app.Bootstrap("reviewer", "synthetic-session-password"); err != nil {
		t.Fatal(err)
	}
	return app
}
