package board

import (
	"slices"
	"testing"
)

func TestFollowupHealthyCompleteThresholdFailurePersistsAfterPass(t *testing.T) {
	fx := followupRetainedFailure(t)
	assessment, err := fx.app.Assess(secondActor, secondProject, fx.candidate.ID)
	if err != nil || assessment.Status != "BLOCKED" || assessment.LedgerHighWater != 2 || len(assessment.Behaviors) != 1 {
		t.Fatalf("existing failed run no longer blocks this healthy candidate: %s, %v", assessment.Status, err)
	}
	behavior := assessment.Behaviors[0]
	if behavior.Status != "FAIL" || behavior.CurrentRunID != "CURRENT" || len(behavior.Runs) != 2 ||
		!slices.ContainsFunc(behavior.Runs, func(run RunAssessment) bool {
			return run.ID == "ARCHIVED" && run.Complete && run.Failed > 0 && run.Definitive
		}) ||
		!slices.ContainsFunc(behavior.Runs, func(run RunAssessment) bool { return run.ID == "CURRENT" && run.Complete && run.Passed == run.Planned }) ||
		!slices.ContainsFunc(behavior.Reasons, func(reason Reason) bool { return reason.Code == "RATE_BELOW_THRESHOLD" && reason.RunID == "ARCHIVED" }) {
		t.Fatalf("complete older rate failure and later complete PASS not both retained: %+v", behavior)
	}
	t.Log(`REB_ACCEPTANCE_INSTANCE {"id":"AC-007-02.P001","case_id":"AC-007-02","parameters":{},"actual":"FAIL persists with RATE_BELOW_THRESHOLD"}`)
}
