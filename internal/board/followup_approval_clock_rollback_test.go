package board

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFollowupApprovalSerializationFailureCannotReserveOrPublishBaseline(t *testing.T) {
	f := newIntakeCustodyFixture(t)
	roster := Baseline{ProductID: "DEMO", ID: "CLOCK-BASELINE", Revision: 1, Behaviors: []Behavior{{
		ID: "CLOCK-BEHAVIOR", Revision: 1, Required: true, Text: "Approval must commit with its provenance",
		Policy: Policy{ID: "CLOCK-POLICY", Revision: 1, MinPassBasisPoints: 10000, MaxAgeHours: 24},
		Tests:  []TestPlan{{ID: "CLOCK-TEST", ExpectedOutcome: "Unverified evidence cannot be sufficient", MustPass: true, PlannedAttempts: 1}},
	}}}
	draft, err := f.app.CreateDraft(secondActor, secondProject, roster, DraftOptions{})
	if err != nil {
		t.Fatal(err)
	}
	approvalsBefore, draftsBefore, err := f.app.Baselines(secondActor, secondProject)
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore, err := f.app.Audit(secondActor, secondProject)
	if err != nil {
		t.Fatal(err)
	}
	database := f.app.db.Path()
	if err := f.app.Close(); err != nil {
		t.Fatal(err)
	}
	armed := false
	invalidRead := 0
	app, err := Open(database, Settings{ProjectID: secondProject, ProductID: "DEMO", Clock: func() time.Time {
		if armed {
			armed = false
			invalidRead++
			return time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		return f.at
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	armed = true
	refused, err := app.Approve(secondActor, secondProject, draft.ID, draft.Revision)
	var marshalError *json.MarshalerError
	if invalidRead != 1 || armed || !errors.As(err, &marshalError) || !strings.Contains(marshalError.Error(), "year outside") {
		t.Fatalf("approval did not report its one-shot timestamp serialization failure: reads=%d, err=%v", invalidRead, err)
	}
	if refused.Descriptor.Digest == "" {
		t.Fatal("fixture did not reach the approval serialization boundary")
	}
	f.app = app
	reopened := f.checkReopened(t, false)
	approvalsAfter, draftsAfter, err := reopened.Baselines(secondActor, secondProject)
	if err != nil || !reflect.DeepEqual(approvalsBefore, approvalsAfter) || !reflect.DeepEqual(draftsBefore, draftsAfter) {
		t.Fatalf("failed approval changed the approved catalog or saved drafts: %v", err)
	}
	saved, err := reopened.Draft(secondActor, secondProject, draft.ID)
	if err != nil || !reflect.DeepEqual(saved, draft) {
		t.Fatalf("failed approval changed its draft: %v", err)
	}
	if _, err := reopened.Baseline(secondActor, secondProject, refused.Descriptor.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed approval exposed an approved digest: %v", err)
	}
	eventsAfter, err := reopened.Audit(secondActor, secondProject)
	if err != nil || len(eventsAfter) != len(eventsBefore)+1 || !reflect.DeepEqual(eventsAfter[:len(eventsBefore)], eventsBefore) {
		t.Fatalf("approval failure changed prior audit or added unexpected events: %v", err)
	}
	failure := eventsAfter[len(eventsBefore)]
	if failure.Action != "baseline.approve" || failure.Actor != secondActor || failure.Outcome != "failure" || !failure.Time.Equal(f.at) {
		t.Fatalf("approval refusal lacked a distinct valid-dated failure event: %#v", failure)
	}
	_, manifest, _, err := reopened.Candidate(secondActor, secondProject, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	input := CandidateInput{Manifest: manifest.Manifest, BaselineDigest: refused.Descriptor.Digest}
	input.Manifest.BuildDigest = strings.Repeat("c", 64)
	if _, _, err := reopened.CreateCandidate(secondActor, secondProject, input); !errors.Is(err, ErrNotFound) {
		t.Fatalf("candidate admitted a refused approval: %v", err)
	}
	approved, err := reopened.Approve(secondActor, secondProject, draft.ID, draft.Revision)
	if err != nil || approved.Descriptor.Digest != refused.Descriptor.Digest || approved.Approver != secondActor || !approved.ApprovedAt.Equal(f.at) {
		t.Fatalf("failed approval reserved immutable identities or poisoned valid retry: %v", err)
	}
	made, reused, err := reopened.CreateCandidate(secondActor, secondProject, input)
	if err != nil || reused || made.ID == f.candidate.ID || made.PredecessorID != f.candidate.ID {
		t.Fatalf("valid approval could not create its linked successor: %v", err)
	}
	assessment, err := reopened.Assess(secondActor, secondProject, made.ID)
	if err != nil || assessment.Status != Blocked || len(assessment.Behaviors) != 1 || assessment.Behaviors[0].Status != Unknown || assessment.Behaviors[0].CurrentRunID != "" {
		t.Fatalf("new baseline inherited sufficient evidence: status=%s err=%v", assessment.Status, err)
	}
}
