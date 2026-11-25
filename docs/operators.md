# Operate a synthetic pilot

Keep the database and backup files private. Use separate accounts for separate responsibilities. Only one process can own a database.

## Prepare operator commands

These Windows procedures require PowerShell 7.1 or later. The application does not require that version for ordinary startup.

1. Stop the server with **Ctrl+C**.
2. Open PowerShell in the extracted `release-evidence-board` folder.
3. Set the executable and database paths.

```powershell
$rebBinary = Resolve-Path -LiteralPath .\bin\windows-amd64\release-evidence-board.exe
$rebDatabase = Join-Path (Get-Location) 'data\evidence.db'
```

Use your existing database path if it differs. The examples use project `pilot`, product `DEMO`, and account `reviewer`. Keep those identities consistent with initialization.

## Assign access

1. Enter the Operator password at the prompt below.
2. Run the account command.
3. Save the generated account password privately.

```powershell
$actorPassword = Read-Host -MaskInput 'Operator password'
$actorPassword | & $rebBinary account -db $rebDatabase -actor reviewer -account contributor -roles contributor
Remove-Variable actorPassword
```

An empty role list removes an account assignment. Viewer and Release Owner can read assigned evidence. Contributor can draft, create candidates, and import. Reviewer can approve and freeze. Operator controls access and recovery. Operator alone cannot approve or revalidate without Reviewer assignment.

## Read or change configuration

Keep the server stopped. Supply the Operator password on standard input.

```powershell
$actorPassword = Read-Host -MaskInput 'Operator password'
$actorPassword | & $rebBinary config -db $rebDatabase -actor reviewer
$actorPassword | & $rebBinary config -db $rebDatabase -actor reviewer -setting backup-interval -duration 24h
Remove-Variable actorPassword
```

| Setting | Default |
| --- | --- |
| Evidence retention | 180 days from candidate creation |
| Audit retention | 180 days from each event |
| Backup expiry | 30 days from backup creation |
| Backup interval | 24 hours |
| Backup overdue warning | 26 hours |
| Import limit | 10 preview/commit requests per account per 60 seconds |

Scheduled backups run while the server runs. Keep their directory under access control.

## Back up

1. Stop the server.
2. Enter the Operator password.
3. Run the backup command.

```powershell
$actorPassword = Read-Host -MaskInput 'Operator password'
$actorPassword | & $rebBinary backup -db $rebDatabase -actor reviewer -backups .\backups
Remove-Variable actorPassword
```

Keep the backup database and adjacent `.sha256` file together. The snapshot contains CSVs, receipts, descriptors, approvals, audit records, and frozen HTML.

## Restore and revalidate

1. Stop the server.
2. Select a verified backup and its checksum file.
3. Choose a new destination path on a filesystem that supports hard links.
4. Replace the example backup filename below.
5. Run restore.

```powershell
& $rebBinary restore -from .\backups\COPY_VERIFIED_BACKUP_FILENAME.db -db .\data\restored.db -project pilot -product DEMO
```

Do not overwrite the original database. A missing checksum, identity mismatch, or integrity failure blocks verified restoration. Protect the destination directory with operating-system permissions.

Restore leaves a review hold. New assessments and exports stay suspended until reviewer revalidation succeeds.

1. Inspect the restored evidence.
2. Enter the Reviewer password.
3. Run revalidation on the restored database.

```powershell
$actorPassword = Read-Host -MaskInput 'Reviewer password'
$actorPassword | & $rebBinary revalidate -db .\data\restored.db -actor reviewer
Remove-Variable actorPassword
```

An interrupted restore can leave a private `.reb-restore-*` staging directory. Confirm that its process has stopped before cleanup. Use only the requested destination for review. Keep original backup and incident records.

A checksum on a derived backup does not clear quarantine from an earlier integrity incident. Recovery needs independently verified evidence and reviewer revalidation.

## Rehearse recovery

Use a clean destination for the drill. Check a retained receipt and frozen-report digest. Record elapsed time and recovered snapshot age.

The targets are a 24-hour recovery point and a four-hour recovery time. A local rehearsal does not establish the qualified reference-host acceptance workload.

```powershell
$actorPassword = Read-Host -MaskInput 'Operator password'
$actorPassword | & $rebBinary restore-drill -db $rebDatabase -actor reviewer -from .\data\restored.db
Remove-Variable actorPassword
```

## Purge retained evidence

1. Stop the server.
2. Enter the Operator password.
3. Preview the candidate scope.

```powershell
$actorPassword = Read-Host -MaskInput 'Operator password'
$actorPassword | & $rebBinary purge -db $rebDatabase -actor reviewer -candidates C-ID-1,C-ID-2
```

Review eligible candidates, original CSVs, receipts, shared-file blockers, and audit scope. Shared bytes stay until all referenced candidates are eligible and selected.

Repeat the same command with `-confirm` set to the preview's exact confirmation digest. Obtain a new preview after any state or scope change. Remove the password variable after use.

Backup expiry uses `expire-backups -db $rebDatabase -actor reviewer -backups .\backups` with the password on standard input. An interrupted expiry retains its intent for the next attempt. Keep reported errors for diagnosis.

Purge and backup expiry need an active, verified store. A downloaded report has separate custody outside the database.

## Linux operator commands

Use the same CLI subcommands and flags. Use `./bin/linux-amd64/release-evidence-board` as the executable. Use `./data/evidence.db` as the sample database.

For example, Bash can supply a password without displaying it:

```bash
read -r -s -p 'Operator password: ' actor_password
printf '\n'
printf '%s\n' "$actor_password" | ./bin/linux-amd64/release-evidence-board backup -db ./data/evidence.db -actor reviewer -backups ./backups
unset actor_password
```

Use a Reviewer password for revalidation. Restart the server with the selected verified database after operator work finishes.
