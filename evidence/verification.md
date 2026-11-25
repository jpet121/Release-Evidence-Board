# Verification results and limits

## Exact build

The released executables come from application revision `32ba36c783c46726a25b40b4bec8cf7267178af4`. [SOURCE_IDENTITY.json](SOURCE_IDENTITY.json) identifies the application and tests used for the qualified measurements. The [prerelease tag](https://github.com/jpet121/Release-Evidence-Board/tree/v1.0.0-m1-rc.1) preserves those files. The public workflow now measures the native path limit for two Windows test fixtures. The application source and released executables retain their original identities.

| Artifact | SHA-256 |
| --- | --- |
| Windows amd64 executable | `3b7207d82115ffe62a1b3babb60db8301d28c7f07485ec2f1e0cf1f99d51b905` |
| Linux amd64 executable | `a8e2a784fd4c6322e55ee5645941db260094667bd17e37762651c5eee777b997` |
| Preserved original engineering ZIP | `21e9ad23751d55485c291d1f13239eaa046ddddbb942d753a807cb60b7fd99f1` |

Public release archives have new packaging and separate checksums. Their executable bytes match the verified artifacts. The original 99,831,828-byte engineering ZIP remains preserved locally. No local database is included.

## Replay of the published Linux executable

The Linux executable listed above produced two frozen reports on November 25, 2026. The reviewer approved baseline B1 and imported synthetic CSVs through the local application. Candidate R1 retained its must-pass failure after a passing rerun. Candidate R2 used the corrected environment E2 and received a SUFFICIENT assessment after a fresh pass. Each report contains its own CSV row references and digests.

| Frozen report | Expected assessment | SHA-256 of the HTML file |
| --- | --- | --- |
| [blocked-rerun-report.html](../examples/blocked-rerun-report.html) | BLOCKED | `2504fe223f2869281e99c874d3112f5de0c40e1b8a88b83ee9748fd5168d02cd` |
| [corrected-successor-report.html](../examples/corrected-successor-report.html) | SUFFICIENT | `fcb41956dbccff9b08bf5c1b08871cf569c60215843c4657b80f66a036662347` |

## Measured production coverage

Ticket 80 closed at verified metadata revision `00e53731938780ae4faaf3870bc4e5ef53ea08fe`. Source manifest: `aa74895f247b81b3b3f575a3fa4f232fa3f2ce2dc89fe8f1319cd80edf3b66a8`.

| Scope | Target or client | Lines | Branch outcomes | Exclusions |
| --- | --- | --- | --- | --- |
| Go and HTML templates | Windows amd64 | 4,139 / 4,139 | 3,974 / 3,974 | 0 |
| Go and HTML templates | Linux amd64 | 4,139 / 4,139 | 3,974 / 3,974 | 0 |
| CSS | Windows Chrome | 74 / 74 | 6 / 6 | 0 |
| CSS | Windows Edge | 74 / 74 | 6 / 6 | 0 |
| CSS | Linux Chrome | 74 / 74 | 6 / 6 | 0 |
| CSS | Linux Firefox | 74 / 74 | 6 / 6 | 0 |

These are retained executions, not a new measurement of documentation. The production inventory has 28 Go files, two templates, and one external CSS file. Inline CSS has its own qualified scope. Do not add it again to the physical source-line count. A zero branch denominator is reviewed as such; it is not an exclusion.

The qualified Go adapter records executable lines and actual decision outcomes. The template adapter records actions, conditions, short-circuit operands, iterations, and natural exhaustion. The owner approved the qualified template and CSS methods under AC-024-06. Native Go statement coverage is supplemental.

[Coverage records](coverage/CURRENT_COVERAGE_RECORDS.json) give derived observation summaries and per-file counts. The index also supplies original-record hashes and measurement source files. [The export index](coverage/EXPORT_INDEX.json) identifies original and public-copy hashes. Public measurements omit workstation paths and private receipt history. Original command logs remain preserved locally. This export does not recreate the original workstation.

## Functional and fault verification

Retained Windows and Linux ordinary suites, applicable race checks, vet, vulnerability checks, state/model checks, fuzz work, and four-client browser journeys passed in their recorded scopes. The local engineering closeout identifies the original results and reuse. Original failures and missing observations remain separate.

The clean-extracted Windows workflow passed 28 checkpoints. It exercised baseline approval, candidate creation, import preview and commit, assessment, freezing, download, and offline report use. The later clean-start check passed seven startup, sign-in, and health assertions for the same executable.

The original public CI run exposed two setup problems. Linux lacked the dedicated memory filesystem for the disk-full test. On the hosted Windows runner, a fixed path length caused backup and restore to fail before their intended operations. The public Windows fixtures now measure the usable path length on the test volume. They require the real backup rename or restore file open to fail and reproduce the same error independently. [Original CI run](https://github.com/jpet121/Release-Evidence-Board/actions/runs/45651954815)

The native fault results include controlled Windows write-lock and Linux file-size-limit failures. These prove the tested software safeguards. The cause and exact errno of the original USB interruption remain unverified.

| Risk | Representative production test |
| --- | --- |
| A later pass hides an earlier definitive failure | `TestFollowupDefinitiveFailureSurvivesRetryAndRemainsSeparateFromSuccessor` in [internal/board/followup_decision_edges_test.go](https://github.com/jpet121/Release-Evidence-Board/blob/main/internal/board/followup_decision_edges_test.go) |
| A partial purge removes shared evidence | `TestFollowupSharedPurgeRefusesPartialThenAuditsWholeScope` in [internal/board/followup_expiry_custody_test.go](https://github.com/jpet121/Release-Evidence-Board/blob/main/internal/board/followup_expiry_custody_test.go) |
| A failed purge publishes partial deletion | `TestFollowupPurgeRollsBackAtIndependentStorageFailures` in [internal/board/followup_purge_rollback_test.go](https://github.com/jpet121/Release-Evidence-Board/blob/main/internal/board/followup_purge_rollback_test.go) |
| Import and freeze disagree about a snapshot | `TestFollowupImportAndFreezeSerializeOneEvidenceSnapshot` in [internal/board/followup_frozen_serialization_test.go](https://github.com/jpet121/Release-Evidence-Board/blob/main/internal/board/followup_frozen_serialization_test.go) |
| A restored store has incomplete evidence custody | `TestFollowupRestoreRefusesHistoricallyIncompleteCustodyDespiteValidChecksum` in [internal/board/followup_custody_test.go](https://github.com/jpet121/Release-Evidence-Board/blob/main/internal/board/followup_custody_test.go) |

Nine retained semantic safeguard mutants per native target were detected. Those 18 targeted detections are not exhaustive mutation adequacy. The owner approved an M1-only exception. The full campaign remains paused.

## Open acceptance evidence

Formal M1 technical acceptance remains **BLOCKED_MISSING_REQUIRED_OBSERVATIONS**. Publication does not change that result.

- Manual accessibility observations are incomplete. Safari and VoiceOver observations are missing. The owner has no Mac access.
- Qualified reference-host capacity and recovery observations are missing. Ordinary Windows/Linux tests do not establish the reference-host workload.
- The original external validator bundle's equivalence remains UNVERIFIED. Tested local validators were accepted for M1.
- Historical workflow FAIL and UNVERIFIED records remain. The owner approved an M1-only historical exception; no chronology was backdated.
- The exhaustive mutation campaign is deferred by owner exception, not marked PASS.

The documented M1 requirements basis, five behavior interpretations, and local validator disposition have recorded owner approvals. Later adoption studies remain at their later stage. Optional AI features stay off. An in-app SUFFICIENT result does not establish application acceptance or deployment authority.

## Repeat ordinary checks

Use Go 1.27.4. From the root, run `go test -count=1 ./...` and `go vet ./...`. On Linux, also run `bash .github/scripts/test-linux-storage.sh` from the repository root. The script uses `sudo` to mount a private 16 MiB memory filesystem. It runs the tagged tests as your current user and unmounts the filesystem on exit. The disk-full test checks that the app preserves the earlier receipt and frozen report after a real write fails. These commands do not reproduce the qualified line/branch campaign. CI uses ordinary checks and does not start full mutation.

## Public export custody

The public snapshot contains source, tests, curated coverage data, and evidence summaries. Private audit history, owner chat, profiles, command logs, and original Git history remain local. Their absence from this export does not exclude production code from measurement. Derived files have new hashes. They are not presented as byte-identical original receipts.

[Public verification summary](VERIFICATION_SUMMARY.json) gives the current dispositions.
