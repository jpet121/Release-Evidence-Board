package main

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupMaintenanceAlertsAtThirdActualTimerCheck(t *testing.T) {
	path := initializeCLI(t)
	app, err := board.Open(path, board.Settings{ProjectID: "pilot", ProductID: "DEMO"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	logs := captureMaintenanceLogs(t)
	backupPath := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			maintenance(ctx, app, backupPath, "")
		}()
		defer func() {
			cancel()
			<-done
		}()
		const alert = "three consecutive application health failures"
		synctest.Wait()
		if strings.Contains(logs.String(), alert) {
			t.Fatal("alert was emitted after only the immediate failed check")
		}
		time.Sleep(59 * time.Second)
		synctest.Wait()
		if strings.Contains(logs.String(), alert) {
			t.Fatal("monitor advanced before the first 60-second tick")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if strings.Contains(logs.String(), alert) {
			t.Fatal("alert was emitted after only two failed checks")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if strings.Count(logs.String(), alert) != 1 {
			t.Fatal("third failed check did not emit one operator alert")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if strings.Count(logs.String(), alert) != 1 {
			t.Fatal("fourth consecutive failure produced a repeated operator alert")
		}
		t.Log(`REB_ACCEPTANCE_INSTANCE {"id":"AC-025-02.P001","case_id":"AC-025-02","parameters":{"consecutiveHealthFailures":3,"healthCheckSeconds":60},"actual":"Operator alert emitted"}`)
	})
}
