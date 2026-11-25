package board_test

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestTicket80EntropyReaderFailureCannotPublishOrChangeAccounts(t *testing.T) {
	if os.Getenv("REB_TICKET80_ENTROPY_CHILD") != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(30 * time.Second)
		if parent, ok := t.Deadline(); ok && parent.Add(-2*time.Second).Before(deadline) {
			deadline = parent.Add(-2 * time.Second)
		}
		ctx, cancel := context.WithDeadline(t.Context(), deadline)
		defer cancel()
		const name = "TestTicket80EntropyReaderFailureCannotPublishOrChangeAccounts"
		child := exec.CommandContext(ctx, executable, "-test.run=^"+name+"$", "-test.timeout=25s", "-test.v")
		child.Env = append(os.Environ(), "REB_TICKET80_ENTROPY_CHILD=1")
		child.WaitDelay = time.Second
		output, err := child.CombinedOutput()
		t.Log(string(output))
		if err != nil {
			t.Fatalf("owned entropy child failed and was joined: %v", err)
		}
		for _, name := range []string{"bootstrap", "account-update"} {
			if !strings.Contains(string(output), "--- PASS: TestTicket80EntropyReaderFailureCannotPublishOrChangeAccounts/"+name+" (") {
				t.Fatal("child did not complete both public operation cases")
			}
		}
		return
	}
	for _, action := range []string{"bootstrap", "account-update"} {
		t.Run(action, func(t *testing.T) {
			app, err := board.Open(filepath.Join(t.TempDir(), "entropy.db"), board.Settings{ProjectID: project, ProductID: "DEMO"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := app.Close(); err != nil {
					t.Error(err)
				}
			})
			file, err := os.CreateTemp(t.TempDir(), "entropy-source-")
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := file.Read(make([]byte, 16)); !errors.Is(err, os.ErrClosed) {
				t.Fatal("owned closed reader did not produce its actual read error")
			}
			withFailedReader := func(operation func() error) error {
				original := rand.Reader
				rand.Reader = file
				defer func() { rand.Reader = original }()
				return operation()
			}
			checkReadError := func(err error) {
				t.Helper()
				var native *os.PathError
				if !errors.As(err, &native) || native.Op != "read" || native.Path != file.Name() || !errors.Is(err, os.ErrClosed) {
					t.Fatal("public operation lost the configured reader failure")
				}
			}
			if action == "bootstrap" {
				checkReadError(withFailedReader(func() error { return app.Bootstrap(supervisor, testPassword) }))
				if _, err := app.Account(supervisor); !errors.Is(err, board.ErrNotFound) {
					t.Fatal("failed hash published a bootstrap account")
				}
				if err := app.Bootstrap(supervisor, testPassword); err != nil {
					t.Fatal(err)
				}
				if _, err := app.Authenticate(supervisor, testPassword); err != nil {
					t.Fatal("restored reader did not recover bootstrap")
				}
				audit, err := app.Audit(supervisor, project)
				if err != nil || len(audit) != 1 || audit[0].Action != "access.bootstrap" || audit[0].Outcome != "success" {
					t.Fatal("failed bootstrap left a spurious success audit")
				}
				return
			}
			if err := app.Bootstrap(supervisor, testPassword); err != nil {
				t.Fatal(err)
			}
			const oldPassword, newPassword = "synthetic-viewer-old-password", "synthetic-viewer-new-password"
			if err := app.SetAccount(supervisor, "member", oldPassword, []board.Role{board.Viewer}); err != nil {
				t.Fatal(err)
			}
			before, err := app.Account("member")
			if err != nil {
				t.Fatal(err)
			}
			checkReadError(withFailedReader(func() error {
				return app.SetAccount(supervisor, "member", newPassword, []board.Role{board.Reviewer, board.Operator})
			}))
			after, err := app.Account("member")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("failed hash changed existing account authority")
			}
			if _, err := app.Authenticate("member", oldPassword); err != nil {
				t.Fatal("failed hash destroyed prior password")
			}
			if _, err := app.Authenticate("member", newPassword); !errors.Is(err, board.ErrForbidden) {
				t.Fatal("failed hash accepted proposed password")
			}
			audit, err := app.Audit(supervisor, project)
			if err != nil || len(audit) != 3 || audit[2].Actor != supervisor || audit[2].Action != "access.set" || audit[2].Outcome != "failure" {
				t.Fatal("failed update lacked its attributed failure audit")
			}
			if err := app.SetAccount(supervisor, "member", newPassword, []board.Role{board.Reviewer, board.Operator}); err != nil {
				t.Fatal(err)
			}
			accepted, err := app.Authenticate("member", newPassword)
			if err != nil || !reflect.DeepEqual(accepted.Roles, []board.Role{board.Reviewer, board.Operator}) {
				t.Fatal("restored reader failed exact credential/role retry")
			}
			if _, err := app.Authenticate("member", oldPassword); !errors.Is(err, board.ErrForbidden) {
				t.Fatal("successful replacement retained the old password")
			}
		})
	}
}
