# Try a synthetic release review

Start with the [two completed reports (.zip)](https://github.com/jpet121/Release-Evidence-Board/raw/refs/heads/main/examples/release-review-reports.zip) to inspect the results offline. To repeat the review, use the steps below with a new [local database](run-locally.md). All test results are synthetic.

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

## Prepare a result for import

Open a second terminal in the extracted `release-evidence-board` folder. Use the generator for your system. Enter the four values from the approved baseline and candidate. The first file records a failure on R1. Change the release, manifest, outcome, and filename when the steps below call for a new run.

### Windows PowerShell

Enter your values and run the block.

```powershell
$demoManifest = 'COPY_ACTUAL_MANIFEST_SHA256'
$demoBaseline = 'COPY_ACTUAL_APPROVED_BASELINE_SHA256'
$demoBehaviorRevisionText = 'COPY_ACTUAL_BEH1_REVISION'
$demoPolicyRevisionText = 'COPY_ACTUAL_POL1_REVISION'
if ($demoManifest -cnotmatch '^[a-f0-9]{64}$' -or $demoBaseline -cnotmatch '^[a-f0-9]{64}$' -or $demoBehaviorRevisionText -notmatch '^[1-9][0-9]*$' -or $demoPolicyRevisionText -notmatch '^[1-9][0-9]*$') {
    throw 'Copy the actual approved identities and positive behavior/policy revisions first.'
}
$demoRelease = 'R1'
$demoOutcome = 'FAIL'
$demoFile = 'original-fail.csv'
$demoFinished = [DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ss.ffffffZ')
$demoRun = 'SYNTHETIC-' + [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssfffffffZ')
$demoHeader = 'schema_version,record_type,result_id,product_id,release_id,manifest_digest,baseline_digest,behavior_id,behavior_revision,policy_id,policy_revision,test_id,run_id,attempt_index,finished_at,run_finished_at,outcome,source_uri,failure_excerpt'
$demoRow = "reb-results/1.3,RESULT,$demoRun-RESULT,DEMO,$demoRelease,$demoManifest,$demoBaseline,BEH1,$demoBehaviorRevisionText,POL1,$demoPolicyRevisionText,T1,$demoRun,1,$demoFinished,$demoFinished,$demoOutcome,,"
[IO.File]::WriteAllText((Join-Path (Get-Location) $demoFile), $demoHeader + "`n" + $demoRow + "`n", [Text.UTF8Encoding]::new($false))
```


### Linux Bash

Enter your values and run the block.

```bash
demo_manifest='COPY_ACTUAL_MANIFEST_SHA256'
demo_baseline='COPY_ACTUAL_APPROVED_BASELINE_SHA256'
demo_behavior_revision='COPY_ACTUAL_BEH1_REVISION'
demo_policy_revision='COPY_ACTUAL_POL1_REVISION'
demo_release='R1'
demo_outcome='FAIL'
demo_file='original-fail.csv'
if [[ ! "$demo_manifest" =~ ^[a-f0-9]{64}$ || ! "$demo_baseline" =~ ^[a-f0-9]{64}$ || ! "$demo_behavior_revision" =~ ^[1-9][0-9]*$ || ! "$demo_policy_revision" =~ ^[1-9][0-9]*$ ]]; then
    printf '%s\n' 'Copy the actual approved identities and revisions first.' >&2
else
    demo_finished=$(date -u +'%Y-%m-%dT%H:%M:%S.%6NZ')
    demo_run="SYNTHETIC-$(date -u +'%Y%m%dT%H%M%S%NZ')"
    printf '%s\n' 'schema_version,record_type,result_id,product_id,release_id,manifest_digest,baseline_digest,behavior_id,behavior_revision,policy_id,policy_revision,test_id,run_id,attempt_index,finished_at,run_finished_at,outcome,source_uri,failure_excerpt' > "$demo_file"
    printf 'reb-results/1.3,RESULT,%s-RESULT,DEMO,%s,%s,%s,BEH1,%s,POL1,%s,T1,%s,1,%s,%s,%s,,\n' "$demo_run" "$demo_release" "$demo_manifest" "$demo_baseline" "$demo_behavior_revision" "$demo_policy_revision" "$demo_run" "$demo_finished" "$demo_finished" "$demo_outcome" >> "$demo_file"
fi
```

## Review the original candidate

1. Open R1 and select **Import results**.
2. Choose `original-fail.csv` and preview it. Check the candidate, test ID, and outcome. Commit the preview.
3. Run the same generator again for R1. Keep the same manifest. Change the outcome to `PASS` and the filename to `passing-rerun.csv`. Run the full block to give the new result a fresh run ID and timestamp.
4. Preview and commit the second CSV. Check the receipt and ledger sequence.
5. Open the live assessment. The current run passed one of one planned tests. The required behavior shows **FAIL**, and R1 remains **BLOCKED**. Look for `MUST_PASS_FAILED` under T1.
6. Open **Frozen reports**. Select **Freeze assessment** and download the HTML file.

An exact retry of either CSV returns its original receipt.

## Review the corrected successor

1. Open R1 and select **Record successor**. Use release ID `R2` and environment revision `E2`. Keep the build, application version, and approved baseline. Check that R1 is the predecessor. Record the correction as `Revised synthetic harness from E1 to E2`.
2. Open R2. The required behavior shows **UNKNOWN**. Record the new manifest digest under **Candidate lineage and descriptors**.
3. Run the generator again. Set the release to `R2`, use the R2 manifest digest, set the outcome to `PASS`, and use the filename `corrected-run.csv`. Keep the approved baseline digest and revisions.
4. Preview and commit the CSV to R2. The current assessment shows **SUFFICIENT** with one passing behavior.
5. Freeze and download the R2 report. Open both downloaded files offline. Follow BEH1 to its test results and CSV row numbers. R1 still shows **BLOCKED**.

Each report records the assessment at the time the reviewer froze it. Live evidence can later become stale.
