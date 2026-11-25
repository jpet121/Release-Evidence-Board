package board_test

import "testing"

// Call only from the completed and case-specific assertions of these tests.
// The lookup attaches the frozen parameter to a real observation.
func recordCorroboratedBoardObservation(t *testing.T) {
	t.Helper()
	if t.Failed() {
		return
	}
	for _, row := range corroboratedAcceptance[t.Name()] {
		recordApprovalInstance(t, row.id, row.parameters, row.result)
	}
}

var corroboratedAcceptance = map[string][]oracleAcceptanceRow{
	"TestFollowupAuthorizedMaterialAuditAttributesExactActorTimeAndRecord": {
		{"AC-012-01.P001", map[string]any{"authorizedAction": "approval"}, "Actor, UTC time, action, references and outcome recorded"},
		{"AC-012-01.P003", map[string]any{"authorizedAction": "candidate creation"}, "Actor, UTC time, action, references and outcome recorded"},
		{"AC-012-01.P006", map[string]any{"authorizedAction": "purge"}, "Actor, UTC time, action, references and outcome recorded"},
		{"AC-012-04.P001", map[string]any{}, "Same author and approver shown"},
	},
	"TestFollowupC03FrozenReviewAndLinkedSourceAreSelfContained": {
		{"AC-010-01.P001", map[string]any{}, "All C03 required fields present with correct values and anchors"},
	},
	"TestFollowupCSVFlavorRetainsSourceAndParsedValues/bom": {
		{"AC-004-13.P001", map[string]any{"csvFeature": "UTF-8 BOM"}, "Correct parsed values and original bytes retained"},
	},
	"TestFollowupCSVFlavorRetainsSourceAndParsedValues/crlf": {
		{"AC-004-13.P003", map[string]any{"csvFeature": "CRLF"}, "Correct parsed values and original bytes retained"},
	},
	"TestFollowupCSVFlavorRetainsSourceAndParsedValues/embedded-newline": {
		{"AC-004-13.P006", map[string]any{"csvFeature": "embedded quoted newline"}, "Correct parsed values and original bytes retained"},
	},
	"TestFollowupCSVFlavorRetainsSourceAndParsedValues/escaped-quote": {
		{"AC-004-13.P005", map[string]any{"csvFeature": "escaped double quote"}, "Correct parsed values and original bytes retained"},
	},
	"TestFollowupCSVFlavorRetainsSourceAndParsedValues/lf-and-empty-optionals": {
		{"AC-004-13.P002", map[string]any{"csvFeature": "LF"}, "Correct parsed values and original bytes retained"},
		{"AC-004-17.P001", map[string]any{}, "Accepted"},
	},
	"TestFollowupCSVFlavorRetainsSourceAndParsedValues/quoted-comma": {
		{"AC-004-13.P004", map[string]any{"csvFeature": "quoted comma"}, "Correct parsed values and original bytes retained"},
	},
	"TestFollowupCSVHeaderOnlyReorderAndGappedSlots": {
		{"AC-004-04.P001", map[string]any{}, "No-op receipt; no invented evidence"},
		{"AC-004-09.P001", map[string]any{"plannedAttempts": 3, "submittedIndexes": []int{1, 3}, "expectedMissingIndex": 2}, "Stored as incomplete; gap listed"},
		{"AC-005-04.P001", map[string]any{"newFileTransform": "reorder rows"}, "No-op receipt; no new run/result"},
	},
	"TestFollowupCSVMaximumFieldValuesSurviveCommit": {
		{"AC-004-18.P001", map[string]any{"idCharacters": 80, "uriCharacters": 2048, "excerptScalars": 4000}, "Accepted"},
	},
	"TestFollowupCSVWholeFileSyntaxAndCapacityRefusal/duplicate-header": {
		{"AC-004-11.P004", map[string]any{"headerDefect": "duplicated"}, "Whole file rejected"},
	},
	"TestFollowupCSVWholeFileSyntaxAndCapacityRefusal/extra-header": {
		{"AC-004-11.P001", map[string]any{"headerDefect": "extra"}, "Whole file rejected"},
	},
	"TestFollowupCSVWholeFileSyntaxAndCapacityRefusal/malformed-quote-after-valid": {
		{"AC-004-10.P003", map[string]any{"syntaxDefect": "malformed quoting"}, "Whole file rejected with logical row/physical line and column where identifiable"},
	},
	"TestFollowupCSVWholeFileSyntaxAndCapacityRefusal/missing-header": {
		{"AC-004-11.P002", map[string]any{"headerDefect": "missing"}, "Whole file rejected"},
	},
	"TestFollowupCSVWholeFileSyntaxAndCapacityRefusal/one-byte-over": {
		{"AC-004-15.P001", map[string]any{"bytes": 10485761}, "Rejected before commit"},
	},
	"TestFollowupCSVWholeFileSyntaxAndCapacityRefusal/one-row-over": {
		{"AC-004-15.P002", map[string]any{"dataRows": 10001}, "Rejected before commit"},
	},
	"TestFollowupCSVWholeFileSyntaxAndCapacityRefusal/reordered-header": {
		{"AC-004-11.P003", map[string]any{"headerDefect": "reordered"}, "Whole file rejected"},
	},
	"TestFollowupCSVWholeFileSyntaxAndCapacityRefusal/wrong-column-count-after-valid": {
		{"AC-004-10.P004", map[string]any{"syntaxDefect": "incorrect column count"}, "Whole file rejected with logical row/physical line and column where identifiable"},
	},
	"TestFollowupCurrentRunCannotCrossCandidateOrBehavior": {
		{"AC-006-08.P001", map[string]any{"nonmatchingScope": "different candidate"}, "It cannot become the current run for the target"},
		{"AC-006-08.P002", map[string]any{"nonmatchingScope": "different behavior"}, "It cannot become the current run for the target"},
	},
	"TestFollowupEachBlockingStatusKeepsItsOwnRequiredCount/FAIL": {
		{"AC-008-02.P001", map[string]any{"singleRequiredStatus": "FAIL"}, "BLOCKED with correct separate counts"},
	},
	"TestFollowupEachBlockingStatusKeepsItsOwnRequiredCount/STALE": {
		{"AC-008-02.P003", map[string]any{"singleRequiredStatus": "STALE"}, "BLOCKED with correct separate counts"},
	},
	"TestFollowupEachBlockingStatusKeepsItsOwnRequiredCount/UNKNOWN": {
		{"AC-008-02.P002", map[string]any{"singleRequiredStatus": "UNKNOWN"}, "BLOCKED with correct separate counts"},
	},
	"TestFollowupEqualCompleteRunsUseLedgerOrdering": {
		{"AC-006-01.P001", map[string]any{"inputRunOrder": []string{"z", "a"}, "expectedLedgerOrder": []string{"a", "z"}}, "a receives the lower sequence. z is current by the ledger tie-break rule"},
	},
	"TestFollowupExactThresholdsAndImmediateMustPassFailure/must-pass-fail-and-missing": {
		{"AC-007-01.P001", map[string]any{}, "FAIL with MUST_PASS_FAILED and missing-slot reasons"},
	},
	"TestFollowupExactThresholdsAndImmediateMustPassFailure/three-of-four-at-cut": {
		{"AC-007-09.P001", map[string]any{"planned": 4, "passes": 3, "thresholdBasisPoints": 7500}, "PASS"},
	},
	"TestFollowupExactThresholdsAndImmediateMustPassFailure/three-of-four-one-bps-over": {
		{"AC-007-10.P001", map[string]any{"planned": 4, "passes": 3, "thresholdBasisPoints": 7501}, "FAIL; displayed rounding irrelevant"},
	},
	"TestFollowupExactThresholdsAndImmediateMustPassFailure/zero-threshold-mandatory-fail": {
		{"AC-007-12.P001", map[string]any{"thresholdBasisPoints": 0, "mustPassSlotOutcome": "FAIL"}, "FAIL"},
	},
	"TestFollowupFailedAuthorizedDraftMutationAppendsOnlyFailure": {
		{"AC-012-03.P002", map[string]any{"failedMutation": "authorized operation failure"}, "No success event; denial/failure outcome distinguishable"},
	},
	"TestFollowupImportAndFreezeSerializeOneEvidenceSnapshot/import-after-snapshot": {
		{"AC-010-03.P002", map[string]any{"serialization": "concurrent import after freeze snapshot"}, "One consistent ledger high-water mark; no mixed snapshot"},
	},
	"TestFollowupImportAndFreezeSerializeOneEvidenceSnapshot/import-before-snapshot": {
		{"AC-010-03.P001", map[string]any{"serialization": "concurrent import before freeze snapshot"}, "One consistent ledger high-water mark; no mixed snapshot"},
	},
	"TestFollowupIndependentUnreachableDenominatorGapsPersist/unusable-false": {
		{"AC-007-22.P001", map[string]any{"plannedAttempts": 10, "requiredPasses": 8, "nonMustPassFailures": 3, "remainingEvidenceGap": "missing slot"}, "FAIL persists. The earlier run reports RATE_BELOW_THRESHOLD with a best possible score of seven out of ten. The missing or unusable evidence and both runs remain visible"},
	},
	"TestFollowupIndependentUnreachableDenominatorGapsPersist/unusable-true": {
		{"AC-007-22.P002", map[string]any{"plannedAttempts": 10, "requiredPasses": 8, "nonMustPassFailures": 3, "remainingEvidenceGap": "unusable result"}, "FAIL persists. The earlier run reports RATE_BELOW_THRESHOLD with a best possible score of seven out of ten. The missing or unusable evidence and both runs remain visible"},
	},
	"TestFollowupLaterCompleteRunDoesNotMergeOrEraseIncompleteHistory": {
		{"AC-006-06.P001", map[string]any{}, "The later-finished run supplies completeness. The other run stays in history"},
		{"AC-007-05.P001", map[string]any{}, "PASS if fresh"},
	},
	"TestFollowupNoEvidenceCountsEveryApprovedRequiredBehavior": {
		{"AC-008-04.P001", map[string]any{}, "All required behaviors counted UNKNOWN"},
	},
	"TestFollowupProducerCloseWinsBothUploadOrders/zero-false/later-first-false": {
		{"AC-006-02.P001", map[string]any{"uploadOrder": []string{"earlier finish", "later finish"}}, "The run with the latest producer finish time is current in both orders"},
		{"AC-006-09.P001", map[string]any{"uploadOrder": []string{"complete", "incomplete"}, "completeRunClose": "09:15:00Z", "incompleteLastResult": "09:05:00Z", "incompleteRunClose": "09:30:00Z"}, "The incomplete run is current in both orders. The missing test remains visible and the older pass stays in history"},
	},
	"TestFollowupProducerCloseWinsBothUploadOrders/zero-false/later-first-true": {
		{"AC-006-02.P002", map[string]any{"uploadOrder": []string{"later finish", "earlier finish"}}, "The run with the latest producer finish time is current in both orders"},
		{"AC-006-09.P002", map[string]any{"uploadOrder": []string{"incomplete", "complete"}, "completeRunClose": "09:15:00Z", "incompleteLastResult": "09:05:00Z", "incompleteRunClose": "09:30:00Z"}, "The incomplete run is current in both orders. The missing test remains visible and the older pass stays in history"},
	},
	"TestFollowupProducerCloseWinsBothUploadOrders/zero-true/later-first-false": {
		{"AC-006-10.P001", map[string]any{"uploadOrder": []string{"passing", "RUN_ONLY"}}, "The zero-result run is current and UNKNOWN with every planned slot missing. The candidate is BLOCKED and the earlier pass stays in history"},
	},
	"TestFollowupProducerCloseWinsBothUploadOrders/zero-true/later-first-true": {
		{"AC-006-10.P002", map[string]any{"uploadOrder": []string{"RUN_ONLY", "passing"}}, "The zero-result run is current and UNKNOWN with every planned slot missing. The candidate is BLOCKED and the earlier pass stays in history"},
	},
	"TestFollowupProducerRecordingPrecedesSuccessorAndPreservesPriorPair": {
		{"AC-004-23.P001", map[string]any{}, "CSV retains the producer values; preview identifies the prior pair and the run cannot satisfy the open candidate"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/cross-product": {
		{"AC-004-20.P001", map[string]any{}, "Entire batch rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/duplicate-run-only": {
		{"AC-004-26.P001", map[string]any{"invalidRun": "duplicate RUN_ONLY"}, "Reject the whole file and keep earlier records intact"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/empty-run-only-1": {
		{"AC-004-16.P018", map[string]any{"recordType": "RUN_ONLY", "emptyField": "schema_version"}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/empty-run-only-10": {
		{"AC-004-16.P026", map[string]any{"recordType": "RUN_ONLY", "emptyField": "policy_id"}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/empty-run-only-11": {
		{"AC-004-16.P027", map[string]any{"recordType": "RUN_ONLY", "emptyField": "policy_revision"}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/empty-run-only-13": {
		{"AC-004-16.P028", map[string]any{"recordType": "RUN_ONLY", "emptyField": "run_id"}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/empty-run-only-16": {
		{"AC-004-16.P029", map[string]any{"recordType": "RUN_ONLY", "emptyField": "run_finished_at"}, "Rejected"},
		{"AC-004-24.P002", map[string]any{"timingDefect": "missing RUN_ONLY run_finished_at"}, "Reject the whole file with the offending timestamp or run identified"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/empty-run-only-2": {
		{"AC-004-16.P019", map[string]any{"recordType": "RUN_ONLY", "emptyField": "record_type"}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/empty-run-only-4": {
		{"AC-004-16.P020", map[string]any{"recordType": "RUN_ONLY", "emptyField": "product_id"}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/empty-run-only-5": {
		{"AC-004-16.P021", map[string]any{"recordType": "RUN_ONLY", "emptyField": "release_id"}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/empty-run-only-6": {
		{"AC-004-16.P022", map[string]any{"recordType": "RUN_ONLY", "emptyField": "manifest_digest"}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/empty-run-only-7": {
		{"AC-004-16.P023", map[string]any{"recordType": "RUN_ONLY", "emptyField": "baseline_digest"}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/empty-run-only-8": {
		{"AC-004-16.P024", map[string]any{"recordType": "RUN_ONLY", "emptyField": "behavior_id"}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/empty-run-only-9": {
		{"AC-004-16.P025", map[string]any{"recordType": "RUN_ONLY", "emptyField": "behavior_revision"}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/inconsistent-release": {
		{"AC-004-06.P002", map[string]any{"identityDefect": "known manifest with inconsistent release_id"}, "Reject the whole file. Import creates no candidate or evidence"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/individually-known-unbound-pair": {
		{"AC-004-06.P003", map[string]any{"identityDefect": "individually known descriptors without a committed pair"}, "Reject the whole file. Import creates no candidate or evidence"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/invalid-policy-revision-+1": {
		{"AC-004-12.P011", map[string]any{"field": "policy_revision", "lexeme": "+1"}, "Whole file rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/invalid-policy-revision--1": {
		{"AC-004-12.P012", map[string]any{"field": "policy_revision", "lexeme": "-1"}, "Whole file rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/invalid-policy-revision-0": {
		{"AC-004-12.P013", map[string]any{"field": "policy_revision", "lexeme": "0"}, "Whole file rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/invalid-policy-revision-01": {
		{"AC-004-12.P014", map[string]any{"field": "policy_revision", "lexeme": "01"}, "Whole file rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/invalid-policy-revision-1e0": {
		{"AC-004-12.P015", map[string]any{"field": "policy_revision", "lexeme": "1e0"}, "Whole file rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/overlong-identifier-10": {
		{"AC-004-19.P005", map[string]any{"field": "policy_id", "characters": 81}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/overlong-identifier-12": {
		{"AC-004-19.P006", map[string]any{"field": "test_id", "characters": 81}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/overlong-identifier-13": {
		{"AC-004-19.P007", map[string]any{"field": "run_id", "characters": 81}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/overlong-identifier-4": {
		{"AC-004-19.P002", map[string]any{"field": "product_id", "characters": 81}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/overlong-identifier-5": {
		{"AC-004-19.P003", map[string]any{"field": "release_id", "characters": 81}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/overlong-identifier-8": {
		{"AC-004-19.P004", map[string]any{"field": "behavior_id", "characters": 81}, "Rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/populated-run-only-12": {
		{"AC-004-26.P005", map[string]any{"illegalPopulatedRunOnlyField": "test_id"}, "Reject the whole file and keep earlier records intact"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/populated-run-only-14": {
		{"AC-004-26.P006", map[string]any{"illegalPopulatedRunOnlyField": "attempt_index"}, "Reject the whole file and keep earlier records intact"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/populated-run-only-15": {
		{"AC-004-26.P007", map[string]any{"illegalPopulatedRunOnlyField": "finished_at"}, "Reject the whole file and keep earlier records intact"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/populated-run-only-17": {
		{"AC-004-26.P008", map[string]any{"illegalPopulatedRunOnlyField": "outcome"}, "Reject the whole file and keep earlier records intact"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/populated-run-only-18": {
		{"AC-004-26.P009", map[string]any{"illegalPopulatedRunOnlyField": "source_uri"}, "Reject the whole file and keep earlier records intact"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/populated-run-only-19": {
		{"AC-004-26.P010", map[string]any{"illegalPopulatedRunOnlyField": "failure_excerpt"}, "Reject the whole file and keep earlier records intact"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/populated-run-only-3": {
		{"AC-004-26.P004", map[string]any{"illegalPopulatedRunOnlyField": "result_id"}, "Reject the whole file and keep earlier records intact"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/result-invalid-close": {
		{"AC-004-12.P003", map[string]any{"valueDefect": "invalid date in run_finished_at"}, "Whole file rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/result-leap-second-close": {
		{"AC-004-12.P005", map[string]any{"valueDefect": "leap second in run_finished_at"}, "Whole file rejected"},
	},
	"TestFollowupRejectInvalidRunOnlyAndIdentityFieldsAtomically/unknown-manifest": {
		{"AC-004-06.P001", map[string]any{"identityDefect": "unknown digest"}, "Reject the whole file. Import creates no candidate or evidence"},
	},
	"TestFollowupRejectMixedExistingRunChangesWithoutPartialImport": {
		{"AC-005-10.P001", map[string]any{}, "Only new runs added atomically; receipt accounts for both"},
	},
	"TestFollowupRejectMixedExistingRunChangesWithoutPartialImport/changed-result": {
		{"AC-005-05.P001", map[string]any{}, "Whole file rejected"},
		{"AC-005-11.P001", map[string]any{}, "No new run committed"},
	},
	"TestFollowupRejectMixedExistingRunChangesWithoutPartialImport/retained-extension": {
		{"AC-005-06.P002", map[string]any{"existingRunSetChange": "extension"}, "Whole file rejected"},
	},
	"TestFollowupRejectMixedExistingRunChangesWithoutPartialImport/retained-subset": {
		{"AC-005-06.P001", map[string]any{"existingRunSetChange": "subset"}, "Whole file rejected"},
	},
}
