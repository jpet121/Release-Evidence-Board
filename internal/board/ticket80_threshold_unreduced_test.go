package board

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestTicket80UnreducedMustPassFailureSurvivesOtherBehaviorReduction(t *testing.T) {
	app, original, at := reviewBoard(t)
	if err := app.SetAccount(secondActor, draftAuthor, "synthetic-independent-reviewer", []Role{Reviewer}); err != nil {
		t.Fatal(err)
	}
	roster := original.Baseline
	roster.ID = "T80-MIXED-REDUCTION"
	roster.Behaviors = append(slices.Clone(roster.Behaviors), Behavior{
		ID: "KEEP", Revision: 1, Required: true, Text: "The independent must-pass safeguard remains required",
		// Zero pass mark isolates the must-pass rule from a rate failure.
		Policy: Policy{ID: "P-KEEP", Revision: 1, MinPassBasisPoints: 0, MaxAgeHours: 24},
		Tests:  []TestPlan{{ID: "T-KEEP", ExpectedOutcome: "Independent safeguard succeeds", MustPass: true, PlannedAttempts: 1}},
	})
	draft, err := app.CreateDraft(secondActor, secondProject, roster, DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	high, err := app.Approve(secondActor, secondProject, draft.ID, draft.Revision)
	if err != nil {
		t.Fatal(err)
	}
	prior := reviewCandidate(t, app, high, "a")
	bhReceipt, _, bhSource := reviewCommit(t, app, prior, reviewRows(prior, "a", "MIXED-BH-OLD", Fail, at))
	keepRows := func(candidate Candidate, run, result string) string {
		return fmt.Sprintf("reb-results/1.3,RESULT,%s,DEMO,release-a,%s,%s,KEEP,1,P-KEEP,1,T-KEEP,%s,1,%s,%s,FAIL,,\n", result, candidate.ManifestDigest, candidate.BaselineDigest, run, FormatUTC(at), FormatUTC(at))
	}
	keepReceipt, _, keepSource := reviewCommit(t, app, prior, keepRows(prior, "MIXED-KEEP-OLD", "KEEP-OLD-RESULT"))
	old, err := app.Freeze(secondActor, secondProject, prior.ID)
	if err != nil || old.Status != Blocked {
		t.Fatalf("fixture lacks the original blocked assessment: %+v %v", old, err)
	}
	_, oldHTML, err := app.ReportHTML(secondActor, secondProject, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	draft, err = app.CreateDraft(draftAuthor, secondProject, high.Baseline, DraftOptions{FromBaseline: high.Descriptor.Digest})
	if err != nil {
		t.Fatal(err)
	}
	lower := draft.Baseline
	lower.Behaviors = slices.Clone(lower.Behaviors)
	lower.Behaviors[0].Policy.MinPassBasisPoints = 5000
	draft, err = app.SaveDraft(secondActor, secondProject, draft.ID, draft.Revision, lower)
	if err != nil {
		t.Fatal(err)
	}
	want := []ThresholdChange{{CandidateID: prior.ID, BehaviorID: "BH", Before: 10000, After: 5000, ReducingActors: []string{secondActor}}}
	if got, err := app.ReviewThresholds(draftAuthor, secondProject, draft.ID); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("unreduced failed behavior contaminated the reduction roster: %+v %v", got, err)
	}
	if _, err := app.Approve(secondActor, secondProject, draft.ID, draft.Revision); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reducing actor approved their own change: %v", err)
	}
	approved, err := app.Approve(draftAuthor, secondProject, draft.ID, draft.Revision)
	if err != nil || !reflect.DeepEqual(approved.Thresholds, want) {
		t.Fatalf("independent review did not preserve the exact single reduction: %+v %v", approved, err)
	}
	successor := reviewCandidate(t, app, approved, "a")
	rows := strings.ReplaceAll(reviewRows(successor, "a", "MIXED-BH-NEW", Fail, at), ",BH,1,P1,1,", ",BH,1,P1,2,")
	reviewCommit(t, app, successor, rows)
	reviewCommit(t, app, successor, keepRows(successor, "MIXED-KEEP-NEW", "KEEP-NEW-RESULT"))
	state, err := app.Assess(secondActor, secondProject, successor.ID)
	statuses := make(map[string]string)
	mandatoryReason := false
	for _, behavior := range state.Behaviors {
		statuses[behavior.ID] = behavior.Status
		if behavior.ID == "KEEP" {
			for _, run := range behavior.Runs {
				for _, reason := range run.Reasons {
					if reason.Code == "MUST_PASS_FAILED" && reason.RunID == "MIXED-KEEP-NEW" && reason.TestID == "T-KEEP" && reason.Attempt == 1 {
						mandatoryReason = true
					}
					if reason.Code == "RATE_BELOW_THRESHOLD" {
						t.Fatal("the zero-threshold fixture also failed its rate rule")
					}
				}
			}
		}
	}
	if err != nil || !mandatoryReason || state.Status != Blocked || !reflect.DeepEqual(statuses, map[string]string{"BH": Pass, "KEEP": Fail}) || state.Counts[Pass] != 1 || state.Counts[Fail] != 1 || state.Counts[Stale] != 0 || state.Counts[Unknown] != 0 {
		t.Fatalf("one approved reduction hid an independent required failure: %+v %v", state, err)
	}
	report, err := app.Freeze(secondActor, secondProject, successor.ID)
	if err != nil || report.Status != Blocked {
		t.Fatalf("frozen successor incorrectly became sufficient: %+v %v", report, err)
	}
	_, successorHTML, err := app.ReportHTML(secondActor, secondProject, report.ID)
	if err != nil {
		t.Fatal(err)
	}
	articleText := func(id string) string {
		t.Helper()
		anchor := `<article id="behavior-` + id + `">`
		if bytes.Count(successorHTML, []byte(anchor)) != 1 {
			t.Fatalf("frozen report lacks a unique behavior article: %s", id)
		}
		_, tail, _ := strings.Cut(string(successorHTML), anchor)
		content, _, closed := strings.Cut(tail, "</article>")
		if !closed {
			t.Fatalf("frozen behavior article did not close: %s", id)
		}
		decoder := xml.NewDecoder(strings.NewReader(anchor + content + "</article>"))
		decoder.Strict = false
		decoder.Entity = xml.HTMLEntity
		var text strings.Builder
		for {
			token, err := decoder.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("could not read public frozen behavior content: %v", err)
			}
			if chars, ok := token.(xml.CharData); ok {
				text.Write(chars)
				text.WriteByte(' ')
			}
		}
		return strings.Join(strings.Fields(text.String()), " ")
	}
	for _, expected := range []struct {
		id     string
		tokens []string
	}{
		{"BH", []string{"BH · PASS", "Required · behavior revision 1", "policy P1 revision 2", "Pass mark 5000 basis points"}},
		{"KEEP", []string{"KEEP · FAIL", "Required · behavior revision 1", "policy P-KEEP revision 1", "Pass mark 0 basis points", "MUST_PASS_FAILED", "T-KEEP attempt 1"}},
	} {
		text := articleText(expected.id)
		for _, token := range expected.tokens {
			if !strings.Contains(text, token) {
				t.Fatalf("frozen behavior %s lost %q: %s", expected.id, token, text)
			}
		}
	}
	for _, source := range []struct {
		receipt Receipt
		raw     []byte
	}{{bhReceipt, bhSource}, {keepReceipt, keepSource}} {
		if got, err := app.SourceCSV(secondActor, secondProject, source.receipt.ID); err != nil || !bytes.Equal(got, source.raw) {
			t.Fatalf("approved reduction changed original source evidence: %v", err)
		}
	}
	if got, html, err := app.ReportHTML(secondActor, secondProject, old.ID); err != nil || got != old || !bytes.Equal(html, oldHTML) {
		t.Fatalf("approved reduction changed the original frozen assessment: %v", err)
	}
}
