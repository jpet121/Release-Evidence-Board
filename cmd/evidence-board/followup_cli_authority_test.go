package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"release-evidence-board/internal/board"
)

func TestFollowupForeignCLIAccessAssignmentPreservesInitializedProject(t *testing.T) {
	path := initializeCLI(t)
	output, err := operatorCLI("account", path, "-project", "foreign", "-account", "new-fixture-viewer", "-roles", "viewer")
	if !errors.Is(err, board.ErrInvalid) || output != "" {
		t.Fatalf("foreign project wrote a new account or exposed credentials: %v", err)
	}
	readAccount := func() (board.Account, error) {
		app, err := board.Open(path, board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
		if err != nil {
			t.Fatal(err)
		}
		account, lookup := app.Account("new-fixture-viewer")
		if err := app.Close(); err != nil {
			t.Fatal(err)
		}
		return account, lookup
	}
	if _, err := readAccount(); !errors.Is(err, board.ErrNotFound) {
		t.Fatalf("foreign CLI mutated the real project account registry: %v", err)
	}
	if _, err := operatorCLI("account", path, "-project", "pilot", "-account", "new-fixture-viewer", "-roles", "viewer"); err != nil {
		t.Fatalf("the valid matching project control could not assign the account: %v", err)
	}
	retained, err := readAccount()
	if err != nil || !slices.Equal(retained.Roles, []board.Role{board.Viewer}) {
		t.Fatalf("valid control did not retain exactly the intended role: %v", err)
	}
	marker, err := json.Marshal(map[string]any{
		"id": "AC-001-03.P023", "case_id": "AC-001-03", "parameters": map[string]any{"requiredOperation": "access assignment"},
		"actual": "No foreign data or mutation is exposed",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("REB_ACCEPTANCE_INSTANCE " + string(marker))
}

func TestFollowupForeignCLIRestoreDoesNotExposeProtectedArchive(t *testing.T) {
	original := initializeCLI(t)
	encoded, err := operatorCLI("backup", original, "-backups", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var backup board.Backup
	if err := json.Unmarshal([]byte(encoded), &backup); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(backup.Path)
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(t.TempDir(), "foreign-restore.db")
	output, err := invokeCLI("restore", foreign, "", "-project", "foreign", "-from", backup.Path)
	if !errors.Is(err, board.ErrInvalid) || output != "" {
		t.Fatalf("foreign project acquired a protected destination: %v", err)
	}
	if _, err := os.Stat(foreign); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign project retained protected destination bytes: %v", err)
	}
	saved, err := os.ReadFile(backup.Path)
	if err != nil || !bytes.Equal(saved, before) {
		t.Fatalf("refused foreign restore altered the protected original: %v", err)
	}
	permitted := filepath.Join(t.TempDir(), "own-project-restore.db")
	output, err = invokeCLI("restore", permitted, "", "-project", "pilot", "-from", backup.Path)
	var restored board.RestoreResult
	if err != nil || json.Unmarshal([]byte(output), &restored) != nil || !restored.IntegrityOK {
		t.Fatalf("matching project control did not restore the same archive: %v", err)
	}
	wire, err := json.Marshal(map[string]any{
		"id": "AC-001-03.P025", "case_id": "AC-001-03",
		"parameters": map[string]any{"requiredOperation": "restore"}, "actual": "No foreign data or mutation is exposed",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("REB_ACCEPTANCE_INSTANCE " + string(wire))
}
