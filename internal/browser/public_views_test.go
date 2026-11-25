package browser_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestBrowserApprovedBaselinesAndOperatorHealth(t *testing.T) {
	app, visit, _ := startTestBrowser(t)
	reviewer, _ := loginSession(t, visit, "reviewer")
	baselines, status := response(t, reviewer.Get, visit+"/p/pilot/baselines")
	if status != http.StatusOK || !strings.Contains(baselines, "B1 · revision 1") ||
		!strings.Contains(baselines, "Download descriptor") || strings.Contains(baselines, "No baseline has been approved") {
		t.Fatalf("approved baselines view did not identify the retained revision: HTTP %d", status)
	}
	health, status := response(t, reviewer.Get, visit+"/p/pilot/health")
	if status != http.StatusOK || !strings.Contains(health, "<dt>Last successful backup</dt><dd>none</dd>") {
		t.Fatalf("operator health misrepresented empty backup history: HTTP %d", status)
	}
	backup, err := app.Backup("reviewer", "pilot", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	health, status = response(t, reviewer.Get, visit+"/p/pilot/health")
	if status != http.StatusOK || !strings.Contains(health, "<dt>Last successful backup</dt><dd>"+backup.CreatedAt.UTC().Format("2006-01-02T15:04:05")) {
		t.Fatalf("operator health did not identify the recorded backup: HTTP %d", status)
	}
	viewer, _ := loginSession(t, visit, "viewer")
	denied, status := response(t, viewer.Get, visit+"/p/pilot/health")
	if status != http.StatusForbidden || strings.Contains(denied, "Last successful backup") {
		t.Fatalf("health disclosed operator data to a viewer: HTTP %d", status)
	}
}
