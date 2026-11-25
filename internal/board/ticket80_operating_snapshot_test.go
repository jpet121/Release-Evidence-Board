package board_test

import (
	"errors"
	"reflect"
	"release-evidence-board/internal/board"
	"strings"
	"testing"
	"time"
)

func TestTicket80OperatingSnapshotKeepsAuthorityAndExactStoredValues(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	config, policy, err := app.OperatingConfigSnapshot(supervisor, project)
	if err != nil || config["backup-interval"] != 24*time.Hour || policy != (board.ImportRatePolicy{Count: 10, Window: time.Minute}) {
		t.Fatalf("snapshot defaults differ: %v %+v %v", config, policy, err)
	}
	config["backup-interval"] = time.Nanosecond
	fresh, _, err := app.OperatingConfigSnapshot(supervisor, project)
	if err != nil || fresh["backup-interval"] != 24*time.Hour {
		t.Fatal("local map mutation changed stored defaults", err)
	}
	if err := app.SetOperatingDuration(supervisor, project, "backup-interval", 48*time.Hour); err != nil {
		t.Fatal(err)
	}
	want := board.ImportRatePolicy{Count: 7, Window: 3 * time.Minute}
	if err := app.SetImportRate(supervisor, project, want); err != nil {
		t.Fatal(err)
	}
	config, policy, err = app.OperatingConfigSnapshot(supervisor, project)
	oldConfig, oldErr := app.OperatingConfig(supervisor, project)
	oldPolicy, rateErr := app.ImportRate(supervisor, project)
	if err != nil || oldErr != nil || rateErr != nil || config["backup-interval"] != 48*time.Hour || policy != want || !reflect.DeepEqual(config, oldConfig) || policy != oldPolicy {
		t.Fatalf("snapshot changed legacy values: %v %+v %v %v %v", config, policy, err, oldErr, rateErr)
	}
	for _, role := range []board.Role{board.Viewer, board.Contributor} {
		actor := string(role)
		if err := app.SetAccount(supervisor, actor, testPassword, []board.Role{role}); err != nil {
			t.Fatal(err)
		}
		denied, p, err := app.OperatingConfigSnapshot(actor, project)
		if !errors.Is(err, board.ErrForbidden) || denied != nil || p != (board.ImportRatePolicy{}) {
			t.Fatalf("nonoperator received a settings snapshot: %v %+v %v", denied, p, err)
		}
	}
	if got, err := app.ImportRate(string(board.Contributor), project); err != nil || got != want {
		t.Fatal("snapshot narrowed existing contributor import-rate authority", err)
	}
	config, policy, err = app.OperatingConfigSnapshot(supervisor, "foreign")
	if !errors.Is(err, board.ErrNotFound) || config != nil || policy != (board.ImportRatePolicy{}) {
		t.Fatal("foreign project exposed snapshot", err)
	}
}

func TestTicket80OperatingSnapshotRejectsEitherDamagedRecordWithoutPartialOutput(t *testing.T) {
	for _, fault := range []string{"rate", "durations", "both"} {
		t.Run(fault, func(t *testing.T) {
			app, path := openBoard(t, func() time.Time { return fixtureTime })
			if err := app.SetOperatingDuration(supervisor, project, "backup-interval", 48*time.Hour); err != nil {
				t.Fatal(err)
			}
			if err := app.SetImportRate(supervisor, project, board.ImportRatePolicy{Count: 7, Window: 3 * time.Minute}); err != nil {
				t.Fatal(err)
			}
			if err := app.Close(); err != nil {
				t.Fatal(err)
			}
			damage := map[string]string{}
			if fault != "durations" {
				damage["import_rate_policy_sha256"] = "damaged"
			}
			if fault != "rate" {
				damage["operating_config_sha256"] = "damaged"
			}
			ticket80WriteMeta(t, path, damage)
			app = ticket80Reopen(t, path)
			config, policy, err := app.OperatingConfigSnapshot(supervisor, project)
			if !errors.Is(err, board.ErrIntegrity) || config != nil || policy != (board.ImportRatePolicy{}) {
				t.Fatalf("damaged snapshot exposed a partial record: %v %+v %v", config, policy, err)
			}
			want := "import rate policy"
			if fault == "durations" {
				want = "operating configuration"
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("rate-before-duration validation order changed: %v", err)
			}
		})
	}
}
