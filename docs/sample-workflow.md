# Try a synthetic release review

This sample uses synthetic data. It does not prove that a producer tested a real build. Use a new database from [Run it](run-locally.md). Keep existing databases unchanged.

## Approve a baseline

1. Sign in as `reviewer`.
2. Open **Baselines**.
3. Enter `B1` in **New baseline ID**.
4. Select **Create draft**.
5. Select **Add behavior**.
6. Enter the following values.

| Field | Value |
| --- | --- |
| Behavior ID | `BEH1` |
| Required behavior | Selected |
| Behavior text | `Reject unauthorized reads` |
| Policy ID | `POL1` |
| Minimum pass basis points | `10000` |
| Maximum age in hours | `24` |
| Test ID | `T1` |
| Planned attempts | `1` |
| Expected outcome | `Access denied` |
| Must pass | Selected |

Keep only that test row. Select **Save behavior**. Check the saved values. Select **Approve saved draft**.

Download the approved descriptor. Record the baseline digest, behavior revision, and policy revision. These revisions can differ from the baseline revision. Use the values in the approved descriptor.

## Create a candidate

1. Open **Candidates**.
2. Select **Create candidate**.
3. Select approved baseline `B1`.
4. Enter these values.

| Field | Value |
| --- | --- |
| Release ID | `R1` |
| Application version | `1.0.0` |
| Build SHA-256 | `ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad` |
| Immutable environment revision | `E1` |
| Optional identities | Keep `not_applicable` |

The build digest is synthetic. Create the candidate. It shows BLOCKED because run evidence is missing.

Open **Candidate lineage and descriptors**. Record the manifest digest and approved baseline digest.

## Prepare a sample CSV

Open a second terminal in the extracted `release-evidence-board` folder. Replace all four placeholders with the recorded values.

### Windows PowerShell

Run these commands:

```powershell
$demoManifest = 'COPY_ACTUAL_MANIFEST_SHA256'
$demoBaseline = 'COPY_ACTUAL_APPROVED_BASELINE_SHA256'
$demoBehaviorRevisionText = 'COPY_ACTUAL_BEH1_REVISION'
$demoPolicyRevisionText = 'COPY_ACTUAL_POL1_REVISION'
if ($demoManifest -cnotmatch '^[a-f0-9]{64}$' -or $demoBaseline -cnotmatch '^[a-f0-9]{64}$' -or $demoBehaviorRevisionText -notmatch '^[1-9][0-9]*$' -or $demoPolicyRevisionText -notmatch '^[1-9][0-9]*$') {
    throw 'Copy the actual approved identities and positive behavior/policy revisions first.'
}
$demoFinished = [DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ss.ffffffZ')
$demoRun = 'SYNTHETIC-' + [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssfffffffZ')
$demoHeader = 'schema_version,record_type,result_id,product_id,release_id,manifest_digest,baseline_digest,behavior_id,behavior_revision,policy_id,policy_revision,test_id,run_id,attempt_index,finished_at,run_finished_at,outcome,source_uri,failure_excerpt'
$demoRow = "reb-results/1.3,RESULT,$demoRun-RESULT,DEMO,R1,$demoManifest,$demoBaseline,BEH1,$demoBehaviorRevisionText,POL1,$demoPolicyRevisionText,T1,$demoRun,1,$demoFinished,$demoFinished,PASS,,"
[IO.File]::WriteAllText((Join-Path (Get-Location) 'demo-pass.csv'), $demoHeader + "`n" + $demoRow + "`n", [Text.UTF8Encoding]::new($false))
```


### Linux Bash

Use this generator instead of the PowerShell commands:

```bash
demo_manifest='COPY_ACTUAL_MANIFEST_SHA256'
demo_baseline='COPY_ACTUAL_APPROVED_BASELINE_SHA256'
demo_behavior_revision='COPY_ACTUAL_BEH1_REVISION'
demo_policy_revision='COPY_ACTUAL_POL1_REVISION'
if [[ ! "$demo_manifest" =~ ^[a-f0-9]{64}$ || ! "$demo_baseline" =~ ^[a-f0-9]{64}$ || ! "$demo_behavior_revision" =~ ^[1-9][0-9]*$ || ! "$demo_policy_revision" =~ ^[1-9][0-9]*$ ]]; then
    printf '%s\n' 'Copy the actual approved identities and revisions first.' >&2
else
    demo_finished=$(date -u +'%Y-%m-%dT%H:%M:%S.%6NZ')
    demo_run="SYNTHETIC-$(date -u +'%Y%m%dT%H%M%S%NZ')"
    printf '%s\n' 'schema_version,record_type,result_id,product_id,release_id,manifest_digest,baseline_digest,behavior_id,behavior_revision,policy_id,policy_revision,test_id,run_id,attempt_index,finished_at,run_finished_at,outcome,source_uri,failure_excerpt' > demo-pass.csv
    printf 'reb-results/1.3,RESULT,%s-RESULT,DEMO,R1,%s,%s,BEH1,%s,POL1,%s,T1,%s,1,%s,%s,PASS,,\n' "$demo_run" "$demo_manifest" "$demo_baseline" "$demo_behavior_revision" "$demo_policy_revision" "$demo_run" "$demo_finished" "$demo_finished" >> demo-pass.csv
fi
```

Both generators create a synthetic row for the exact approved contract.

## Import the result

1. Open R1.
2. Select **Import results**.
3. Choose `demo-pass.csv`.
4. Preview the import.
5. Check the candidate, baseline, behavior, and run.
6. Commit the preview.
7. Check the receipt, original CSV digest, and ledger sequence.
8. Open the candidate's live assessment.

Fresh, complete evidence should show SUFFICIENT, PASS 1, FAIL 0, STALE 0, and UNKNOWN 0. An exact repeat import returns the same receipt. A later pass cannot erase a definitive failure.

## Freeze and read offline

1. Open **Frozen reports**.
2. Select **Freeze assessment**.
3. Download the HTML report.
4. Open the downloaded file locally.
5. Set that browser tab offline.
6. Reload the file.
7. Check the report ID, historical status, and counts.
8. Select BEH1.
9. Confirm that its evidence opens in the same file.
10. Check that no external resource requests failed.
11. Restore **No throttling**.

The frozen status stays historical. Live evidence can later become stale. Do not change identities merely to make a rejected import pass.
