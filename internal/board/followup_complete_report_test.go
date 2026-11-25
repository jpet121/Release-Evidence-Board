package board_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-evidence-board/internal/board"
)

func TestFollowupC03FrozenReviewAndLinkedSourceAreSelfContained(t *testing.T) {
	app, _ := openBoard(t, func() time.Time { return fixtureTime })
	roster := twoRequiredBehaviors()
	const approvedText = `Synthetic <strong data-name="literal">approved behavior</strong>`
	const excerpt = `Retained <em>example</em> & exact source`
	roster.Behaviors[0].Text = approvedText
	first := fixtureCandidate(t, app, roster)
	fixtureFailure := resultRow(first, "T1", "1", "PREDECESSOR-FAIL", "ORIGINAL-BLOCKER", intakeEarlier, intakeEarlier, "FAIL")
	importBytes(t, app, first.ID, csvBytes(t, fixtureFailure))
	const approver = "distinct-report-reviewer"
	if err := app.SetAccount(supervisor, approver, testPassword, []board.Role{board.Reviewer}); err != nil {
		t.Fatal(err)
	}
	draft, err := app.CreateDraft(supervisor, project, roster, board.DraftOptions{FromBaseline: first.BaselineDigest})
	if err != nil {
		t.Fatal(err)
	}
	proposed := draft.Baseline
	proposed.Behaviors[0].Policy.MinPassBasisPoints = 7500
	draft, err = app.SaveDraft(supervisor, project, draft.ID, draft.Revision, proposed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Approve(supervisor, project, draft.ID, draft.Revision); !errors.Is(err, board.ErrForbidden) {
		t.Fatalf("fixture did not exercise independent lowering authority: %v", err)
	}
	approved, err := app.Approve(approver, project, draft.ID, draft.Revision)
	if err != nil || len(approved.Thresholds) != 1 {
		t.Fatalf("higher retained failure did not yield one threshold finding: %v", err)
	}
	const display = "Synthetic successor evidence"
	const correction = "Separate reviewed threshold revision"
	successor, reused, err := app.CreateCandidate(supervisor, project, board.CandidateInput{
		Manifest: vectorManifest(), BaselineDigest: approved.Descriptor.Digest,
		PredecessorID: first.ID, Correction: correction, DisplayVersion: display,
	})
	if err != nil || reused || successor.ID == first.ID {
		t.Fatalf("reviewed revision did not create its own candidate: %v", err)
	}
	matching := func(row []string, behavior board.Behavior) []string {
		row[7], row[8] = behavior.ID, strconv.Itoa(behavior.Revision)
		row[9], row[10] = behavior.Policy.ID, strconv.Itoa(behavior.Policy.Revision)
		return row
	}
	failed := matching(resultRow(successor, "T1", "1", "A-HISTORICAL-FAIL", "FIXTURE-FAILED", intakeMiddle, intakeMiddle, "FAIL"), approved.Baseline.Behaviors[0])
	current := matching(resultRow(successor, "T1", "1", "B-CURRENT-PASS", "FIXTURE-PASS", intakeCurrent, intakeCurrent, "PASS"), approved.Baseline.Behaviors[0])
	missing := matching(runOnly(successor, "C-ZERO-RESULTS", intakeCurrent), approved.Baseline.Behaviors[1])
	failed[17], failed[18] = "https://example.invalid/unarchived-evidence", excerpt
	raw := csvBytes(t, failed, current, missing)
	receipt := importBytes(t, app, successor.ID, raw)
	report, err := app.Freeze(approver, project, successor.ID)
	if err != nil || report.Status != "BLOCKED" || report.LedgerHighWater != 4 {
		t.Fatalf("report did not retain the exact blocked decision at high water 4: %v", err)
	}
	_, body, err := app.ReportHTML(approver, project, report.ID)
	if err != nil {
		t.Fatal(err)
	}
	root, anchors := offlineParse(t, body)
	offlineSafeLinks(t, root, anchors)
	if strings.Contains(string(body), approvedText) || strings.Contains(string(body), excerpt) {
		t.Fatal("closed report interpreted source or approved markup as document markup")
	}
	header := offlineOne(t, "header", root.tagged("header"))
	hasText(t, header.plain(), report.ID, "Historical assessment · BLOCKED", display, successor.ID,
		"2026-11-17T12:00:00Z", "2026-11-18T11:00:00Z", "Frozen by "+approver,
		"reb-rules/1.5", "Ledger high-water mark 4", "No deployment authority",
		"PASS 0", "FAIL 1", "STALE 0", "UNKNOWN 1")
	hasText(t, root.plain(), correction, first.ID, "Independent threshold approvals", "BEH1 · 10000 to 7500 basis points",
		supervisor, "approved by "+approver+" at 2026-11-17T12:00:00Z")
	hasText(t, root.plain(), "Predecessor"+first.ID,
		fmt.Sprintf("Baseline%s · revision %d · approved by %s at 2026-11-17T12:00:00Z", approved.Baseline.ID, approved.Baseline.Revision, approver),
		"Manifest SHA-256"+successor.ManifestDigest, "Baseline SHA-256"+successor.BaselineDigest,
		"Approved behavior, roster or policy")
	approval := offlineOne(t, "threshold approval item", root.matching(func(node *offlineElement) bool {
		return node.tag == "li" && strings.Contains(node.plain(), "BEH1 · 10000 to 7500 basis points")
	}))
	reducers := approval.tagged("strong")
	if len(reducers) != 1 || reducers[0].plain() != supervisor {
		t.Error("threshold approval lost the exact reducing authority")
	}
	for _, descriptor := range []struct {
		id, digest string
	}{
		{"manifest-descriptor", successor.ManifestDigest},
		{"baseline-descriptor", successor.BaselineDigest},
		{"evidence-descriptor", report.EvidenceDigest},
	} {
		anchor := reportAnchor(t, anchors, descriptor.id)
		if hash := fmt.Sprintf("%x", sha256.Sum256([]byte(anchor.plain()))); hash != descriptor.digest {
			t.Fatalf("report %s failed its protected canonical binding", descriptor.id)
		}
	}
	if report.SHA256 != fmt.Sprintf("%x", sha256.Sum256(body)) {
		t.Fatal("independent frozen HTML hash disagrees with the protected record")
	}
	firstBehavior := reportAnchor(t, anchors, "behavior-BEH1")
	contract := approved.Baseline.Behaviors[0]
	hasText(t, firstBehavior.plain(), approvedText, "BEH1 · FAIL", "Required · behavior revision",
		fmt.Sprintf("Required · behavior revision %d · policy POL1 revision %d", contract.Revision, contract.Policy.Revision),
		"Pass mark 7500 basis points · maximum age 24 hours · current pass count 1 of 1 planned",
		"T1 · Access denied · 1 attempts · must-pass true",
		"Current run B-CURRENT-PASS", "RATE_BELOW_THRESHOLD", "MUST_PASS_FAILED")
	secondBehavior := reportAnchor(t, anchors, "behavior-BEH2")
	hasText(t, secondBehavior.plain(), "BEH2 · UNKNOWN", "Required", "Current run C-ZERO-RESULTS", "MISSING_SLOT", "T2 attempt 1")
	if strings.Contains(firstBehavior.plain(), "incomplete run selected at a tied finish time") ||
		strings.Contains(secondBehavior.plain(), "incomplete run selected at a tied finish time") {
		t.Error("frozen review claimed a tie that did not occur")
	}
	section := offlineOne(t, "behavior index section", root.matching(func(node *offlineElement) bool {
		return node.tag == "section" && node.attrs["aria-labelledby"] == "behaviors"
	}))
	var behaviorLinks []string
	for _, child := range section.children {
		if child.tag == "ul" {
			for _, link := range child.tagged("a") {
				behaviorLinks = append(behaviorLinks, link.attrs["href"])
			}
		}
	}
	if !slices.Equal(behaviorLinks, []string{"#behavior-BEH1", "#behavior-BEH2"}) {
		t.Errorf("behavior index omitted the two exact offline destinations: %v", behaviorLinks)
	}
	behaviorReasons := offlineOne(t, "first assessment reason list", firstBehavior.matching(func(node *offlineElement) bool {
		return node.tag == "ul" && node.attrs["aria-label"] == "Assessment reasons"
	}))
	items := behaviorReasons.tagged("li")
	if len(items) != 2 || items[0].plain() != "MUST_PASS_FAILED · A-HISTORICAL-FAIL · T1 attempt 1" {
		t.Error("behavior reason lost must-pass test and attempt attribution")
	}
	for _, item := range items {
		links := item.tagged("a")
		if len(links) != 1 || links[0].attrs["href"] != "#runid-A-HISTORICAL-FAIL" ||
			links[0].plain() != "A-HISTORICAL-FAIL" {
			t.Error("frozen reason did not link the exact originating run")
		}
	}
	historical := reportAnchor(t, anchors, "runid-A-HISTORICAL-FAIL")
	hasText(t, historical.plain(), "ledger sequence 2", intakeMiddle, "definitive failure true",
		"best possible 0 of 1 · pass mark 7500 basis points", receipt.Hash, receipt.ID, "FIXTURE-FAILED · FAIL · CSV row 2")
	hasText(t, reportAnchor(t, anchors, "row-2-2").plain(),
		"logical row 2 · physical line 2", "Test T1 · attempt 1 · result finish "+intakeMiddle+" · outcome FAIL", excerpt)
	hasText(t, reportAnchor(t, anchors, "runid-B-CURRENT-PASS").plain(), "ledger sequence 3", "definitive failure false", "FIXTURE-PASS · PASS")
	missingRun := reportAnchor(t, anchors, "runid-C-ZERO-RESULTS")
	reasonLists := missingRun.matching(func(node *offlineElement) bool {
		return node.tag == "ul" && node.attrs["aria-label"] == ""
	})
	if len(reasonLists) != 1 || reasonLists[0].plain() != "MISSING_SLOT · T2 attempt 1" {
		t.Error("frozen run reason list lost its exact incomplete slot")
	}
	hasText(t, reportAnchor(t, anchors, "row-4-4").plain(), "RUN_ONLY · No result rows", "logical row 4", intakeCurrent)
	foundSource := false
	for _, link := range root.tagged("a") {
		if link.attrs["href"] == failed[17] && link.plain() == "Unarchived source link" {
			foundSource = true
		}
	}
	if !foundSource {
		t.Fatal("protected source link lost its plain unarchived label")
	}
	original, err := app.SourceCSV(approver, project, receipt.ID)
	if err != nil || !bytes.Equal(original, raw) {
		t.Fatalf("report provenance did not preserve the complete original source: %v", err)
	}
	recordCorroboratedBoardObservation(t)
}

func reportAnchor(t *testing.T, anchors map[string]*offlineElement, id string) *offlineElement {
	t.Helper()
	node := anchors[id]
	if node == nil {
		t.Fatalf("closed report lost required anchor %q", id)
	}
	return node
}

func hasText(t *testing.T, text string, values ...string) {
	t.Helper()
	for _, value := range values {
		if !strings.Contains(text, value) {
			t.Errorf("closed report omitted required value %q", value)
		}
	}
}
