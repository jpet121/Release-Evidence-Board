package browser_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"release-evidence-board/internal/board"
)

func TestBrowserDraftEditsShowActualApprovalRelationship(t *testing.T) {
	app, visit, candidate := startTestBrowser(t)
	author, authorCSRF := loginSession(t, visit, "reviewer")
	_, _, selfApproved, err := app.Candidate("reviewer", "pilot", candidate)
	if err != nil {
		t.Fatal(err)
	}

	const peerID = "approval-display-peer"
	if err := app.SetAccount("reviewer", peerID, browserPassword, []board.Role{board.Reviewer}); err != nil {
		t.Fatal(err)
	}
	peer, peerCSRF := loginSession(t, visit, peerID)
	project := visit + "/p/pilot"
	draftPath := submitRedirect(t, author, project+"/drafts", authorCSRF,
		url.Values{"baseline": {selfApproved.Descriptor.Digest}})
	draftID := strings.TrimPrefix(draftPath, "/p/pilot/drafts/")
	draft := loadBrowserDraft(t, app, draftID)
	submitRedirect(t, peer, visit+draftPath+"/approve", peerCSRF,
		url.Values{"revision": {strconv.FormatUint(draft.Revision, 10)}})
	draft = loadBrowserDraft(t, app, draftID)
	separatelyApproved, err := app.Baseline("reviewer", "pilot", draft.ApprovedDigest)
	if err != nil {
		t.Fatal(err)
	}

	body, status := response(t, author.Get, project+"/baselines")
	if status != http.StatusOK {
		t.Fatalf("approved baselines returned HTTP %d", status)
	}
	for _, tc := range []struct {
		name         string
		approved     board.ApprovedBaseline
		approver     string
		selfApproval bool
	}{
		{"self-approved", selfApproved, "reviewer", true},
		{"separate-reviewer", separatelyApproved, peerID, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := tc.approved
			if record.Author != "reviewer" || record.Approver != tc.approver || record.SelfApproval != tc.selfApproval {
				t.Fatalf("stored provenance author=%q approver=%q self-approval=%t", record.Author, record.Approver, record.SelfApproval)
			}
			heading := fmt.Sprintf("%s · revision %d</h3>", record.Baseline.ID, record.Baseline.Revision)
			_, remaining, found := strings.Cut(body, heading)
			row, _, closed := strings.Cut(remaining, "</li>")
			if !found || !closed || !strings.Contains(row, record.Descriptor.Digest) {
				t.Fatal("approved baseline row missing its exact descriptor identity")
			}
			if !strings.Contains(row, "Approved by "+tc.approver+" ·") {
				t.Errorf("approved baseline row did not name approver %q", tc.approver)
			}
			if shown := strings.Contains(row, "self-approval"); shown != tc.selfApproval {
				t.Errorf("approved baseline row shows self-approval=%t, want %t for author %q and approver %q",
					shown, tc.selfApproval, record.Author, record.Approver)
			}
		})
	}
}
