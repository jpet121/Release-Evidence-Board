# How the board keeps evidence

One Go server renders the HTML. The interface has no first-party JavaScript. The bbolt database stores contracts, candidates, result rows, receipts, audit records, and frozen HTML.

## Identify the contract

A baseline defines behaviors and planned tests. Approval fixes the saved contract. A candidate identifies its build, environment, and approved baseline. Canonical descriptors have SHA-256 digests. Producers carry these identities into their results.

Candidate creation needs an approved baseline. The setup view shows NOT_ASSESSABLE before that prerequisite exists.

## Commit an import once

Preview shows the projected assessment. Commit checks the current state again. Original CSV bytes, runs, results, ledger changes, receipt, and publication audit enter one transaction. Failure rolls back that transaction. An exact retry returns the retained receipt.

## Assess the runs

The latest producer finish time selects the current run. At equal times, an incomplete run takes precedence over a complete run. Ledger sequence resolves ties within the same completeness class.

A definitive failure remains attached to the candidate. A later pass cannot remove it. Missing or incomplete current evidence is UNKNOWN. Complete evidence becomes STALE after its allowed age. All required behaviors must pass for SUFFICIENT.

## Freeze an assessment

Freezing reads one evidence snapshot. The HTML contains the contract, evidence, counts, time, digests, and reproduction steps. Behavior links stay in the same file. The report makes no automatic external resource requests. A source link can leave the file when a reader selects it.

## Protect retained records

The server checks roles. Mutating forms need a session and CSRF token. Purge needs an exact confirmation digest. Shared evidence stays while a retained candidate needs it. A write failure cannot leave half a purge committed.

Backups have checksums. Restore checks identity and integrity before publication. Restored stores stay on review hold until reviewer revalidation. [Operator procedures](operators.md) cover custody, recovery, and retention.

Tests use real storage and application interfaces. [Verification](../evidence/verification.md) names representative tests and retained results.
