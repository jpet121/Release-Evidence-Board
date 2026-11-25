package main

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"release-evidence-board/internal/board"
)

func TestCLIConfiguresTypedImportThrottle(t *testing.T) {
	path := initializeCLI(t)
	for _, variant := range [][]string{{"-setting="}, {"-duration="}, {"-setting=", "-duration="}} {
		if _, err := operatorCLI("config", path, variant...); !errors.Is(err, board.ErrInvalid) {
			t.Errorf("explicit empty config option %v returned %v, want ErrInvalid", variant, err)
		}
	}
	for _, entry := range []struct{ actor, role string }{
		{"limited-viewer", "viewer"},
		{"limited-contributor", "contributor"},
		{"assigned-operator", "operator"},
	} {
		out, err := operatorCLI("account", path, "-account", entry.actor, "-roles", entry.role)
		if err != nil {
			t.Fatal(err)
		}
		password := issuedCLIPassword(t, out, entry.actor)
		_, err = invokeCLI("config", path, password+"\n", "-actor", entry.actor, "-setting", "import-rate", "-count", "2", "-duration", "2m")
		if entry.role != "operator" {
			if !errors.Is(err, board.ErrForbidden) {
				t.Errorf("%s import-rate change: %v, want ErrForbidden", entry.role, err)
			}
		} else if err != nil {
			t.Fatalf("authenticated Operator could not configure typed import limit: %v", err)
		}
	}
	raw, err := operatorCLI("config", path)
	var config map[string]string
	if err != nil || json.Unmarshal([]byte(raw), &config) != nil ||
		config["import-rate-count"] != "2" || config["import-rate-window"] != "2m0s" {
		t.Fatalf("zero-option read did not show persisted typed limit after reopen: %q, %v", raw, err)
	}
	for _, flags := range [][]string{
		{"-setting", "import-rate", "-count", "0", "-duration", "2m"},
		{"-setting", "import-rate", "-count", "1ns", "-duration", "2m"},
		{"-setting", "import-rate", "-count", "2", "-duration="},
		{"-setting", "backup-interval", "-count", "2", "-duration", "2m"},
		{"-count="},
	} {
		if _, err := operatorCLI("config", path, flags...); err == nil {
			t.Errorf("bad typed import rate flags %v were accepted", flags)
		}
	}
	app, err := board.Open(path, board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	audit, err := app.Audit("reviewer", "pilot")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"import-rate", "old_count=10", "old_window=1m0s", "new_count=2", "new_window=2m0s"}
	var changes int
	for _, event := range audit {
		if event.Action == "config.change" && event.Outcome == "success" {
			changes++
			if event.Actor != "assigned-operator" || event.Time.IsZero() || !slices.Equal(event.References, want) {
				t.Fatalf("import rate audit = %+v, want refs %v", event, want)
			}
		}
	}
	if changes != 1 {
		t.Fatalf("expected a single successful configuration audit, got %d", changes)
	}
}
