package browser_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestFollowupAssignedPopulatedCandidateListCannotCrossProjects(t *testing.T) {
	_, base, candidate := startTestBrowser(t)
	viewer, _ := loginSession(t, base, "viewer")
	local, status := response(t, viewer.Get, base+"/p/pilot")
	if status != http.StatusOK || !strings.Contains(local, candidate) || !strings.Contains(local, "Active candidates") {
		t.Fatalf("known populated list was not readable in assigned project: HTTP %d", status)
	}
	foreign, status := response(t, viewer.Get, base+"/p/foreign")
	if status != http.StatusNotFound || !strings.Contains(foreign, "The requested record is unavailable.") || strings.Contains(foreign, candidate) || strings.Contains(foreign, "Active candidates") {
		t.Fatalf("foreign candidate list exposed assigned project contents: HTTP %d", status)
	}
	recordThresholdHTTPInstance(t, "AC-001-03.P001", map[string]any{"requiredOperation": "candidate list"}, "No foreign data or mutation is exposed")
}
