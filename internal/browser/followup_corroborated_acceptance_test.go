package browser_test

import "testing"

type browserAcceptanceRow struct {
	id         string
	parameters map[string]any
	result     string
}

// Call only from the completed and case-specific assertions of these tests.
// The lookup attaches the frozen parameter to a real observation.
func recordCorroboratedBrowserObservation(t *testing.T) {
	t.Helper()
	if t.Failed() {
		return
	}
	for _, row := range corroboratedAcceptance[t.Name()] {
		recordThresholdHTTPInstance(t, row.id, row.parameters, row.result)
	}
}

var corroboratedAcceptance = map[string][]browserAcceptanceRow{
	"TestFollowupDirectAuditMutationAttemptsCannotChangeHistory/DELETE": {
		{"AC-012-02.P002", map[string]any{"request": "audit delete"}, "Denied except controlled expiry purge"},
	},
	"TestFollowupDirectAuditMutationAttemptsCannotChangeHistory/PATCH": {
		{"AC-012-02.P001", map[string]any{"request": "audit update"}, "Denied except controlled expiry purge"},
	},
	"TestFollowupEvidenceDetailSurvivesUnavailableExternalSource": {
		{"AC-009-01.P001", map[string]any{}, "Exact manifest/baseline/policy, current run, all historical runs and reasons visible"},
		{"AC-009-02.P001", map[string]any{}, "Local evidence still accessible; no source verification claim"},
		{"AC-009-06.P001", map[string]any{"sourceText": "long text"}, "Plain escaped text; no execution"},
		{"AC-009-06.P002", map[string]any{"sourceText": "malicious markup"}, "Plain escaped text; no execution"},
	},
	"TestFollowupOperatorHealthShowsSafeLiveCountersAndAIState": {
		{"AC-012-05.P001", map[string]any{"excludedContent": "raw payload"}, "No payload, secret or excerpt leakage"},
		{"AC-012-05.P002", map[string]any{"excludedContent": "secret/token"}, "No payload, secret or excerpt leakage"},
		{"AC-012-05.P003", map[string]any{"excludedContent": "source failure excerpt"}, "No payload, secret or excerpt leakage"},
		{"AC-025-01.P001", map[string]any{}, "Import failures, storage/DB state, report failures and backup age visible without payloads"},
		{"AC-025-05.P001", map[string]any{}, "Core health independent; AI shown disabled, not failed"},
	},
	"TestFollowupRunHistoryAndDecisionHaveNoPublicOverrideOperation/deploy": {
		{"AC-008-07.P002", map[string]any{"attempt": "deployment action"}, "No override/deployment capability"},
	},
	"TestFollowupRunHistoryAndDecisionHaveNoPublicOverrideOperation/override": {
		{"AC-008-07.P001", map[string]any{"attempt": "status override"}, "No override/deployment capability"},
	},
	"TestFollowupRunHistoryAndDecisionHaveNoPublicOverrideOperation/runs/delete": {
		{"AC-006-04.P002", map[string]any{"action": "delete"}, "All unavailable or denied"},
	},
	"TestFollowupRunHistoryAndDecisionHaveNoPublicOverrideOperation/runs/invalidate": {
		{"AC-006-04.P004", map[string]any{"action": "invalidate"}, "All unavailable or denied"},
	},
	"TestFollowupRunHistoryAndDecisionHaveNoPublicOverrideOperation/runs/reorder": {
		{"AC-006-04.P003", map[string]any{"action": "reorder"}, "All unavailable or denied"},
	},
	"TestFollowupRunHistoryAndDecisionHaveNoPublicOverrideOperation/runs/select": {
		{"AC-006-04.P001", map[string]any{"action": "select"}, "All unavailable or denied"},
	},
}
