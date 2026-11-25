# Run Release Evidence Board

Use synthetic data for M1 evaluation. Keep your existing pilot database. Use a separate database for a new evaluation.

## Get the build

1. Open [Releases](https://github.com/jpet121/Release-Evidence-Board/releases).
2. Download the Windows amd64 or Linux amd64 archive.
3. Download `SHA256SUMS.txt` from that release.
4. Compare the archive's SHA-256 with the listed value.

Windows PowerShell:

```powershell
Get-FileHash -LiteralPath .\Release-Evidence-Board-M1-Windows.zip -Algorithm SHA256
Expand-Archive -LiteralPath .\Release-Evidence-Board-M1-Windows.zip -DestinationPath .\reb-m1
Set-Location -LiteralPath .\reb-m1\release-evidence-board
```

Linux:

```sh
sha256sum Release-Evidence-Board-M1-Linux.tar.gz
mkdir reb-m1
tar -xzf Release-Evidence-Board-M1-Linux.tar.gz -C reb-m1
cd reb-m1/release-evidence-board
```

## Start on Windows

Run these commands in PowerShell. Keep the generated password in a private place.

```powershell
New-Item -ItemType Directory -Force .\data | Out-Null
if (-not (Test-Path -LiteralPath .\data\evidence.db)) {
    .\bin\windows-amd64\release-evidence-board.exe init -db .\data\evidence.db -project pilot -product DEMO -actor reviewer
    if ($LASTEXITCODE -ne 0) { throw 'Initialization failed. Read the error before starting the server.' }
}
.\bin\windows-amd64\release-evidence-board.exe serve -db .\data\evidence.db -project pilot -product DEMO -listen 127.0.0.1:8080
```

## Start on Linux

```sh
mkdir -p ./data
chmod 700 ./data
if [ ! -e ./data/evidence.db ]; then
    ./bin/linux-amd64/release-evidence-board init -db ./data/evidence.db -project pilot -product DEMO -actor reviewer || exit 1
fi
./bin/linux-amd64/release-evidence-board serve -db ./data/evidence.db -project pilot -product DEMO -listen 127.0.0.1:8080
```

Open **http://127.0.0.1:8080/login**. Enter `reviewer` and the password from initialization. Keep the terminal open. Press **Ctrl+C** in that terminal to stop the server.

Only one process can own a database. Stop that process before operator commands access its database. A restart ends browser sessions. Sign in again after a restart.

If port 8080 is in use, choose an unused port. Change the `-listen` value and browser address together. Keep the same database, project, and product values.

The first account has Reviewer and Operator roles. [Operator procedures](operators.md) explain account assignment. Protect the data directory and password with operating-system permissions. Access outside loopback needs private HTTPS and the `-tls-cert` and `-tls-key` options.

## Try the workflow

Follow [the synthetic sample](sample-workflow.md). You can also download [a sample report](../examples/frozen-report.html) and open it locally. GitHub's source view does not render HTML reports.

## Build from source

Binary archives do not contain the source tree. Install Git and Go **1.27.4** for a source build.

Clone the source into a new folder:

```text
git clone https://github.com/jpet121/Release-Evidence-Board.git
cd Release-Evidence-Board
```

Then run these commands from that repository root:

```text
go mod download
go mod verify
go test -count=1 ./...
go vet ./...
go build -trimpath -buildvcs=false -o evidence-board.exe ./cmd/evidence-board
```

On Linux, use `-o evidence-board`. Use that executable in place of the supplied binary. A rebuild is a new artifact. Record its checksum separately.

## Resolve setup errors

| Symptom | Action |
| --- | --- |
| Password rejected | Check the account, database, project, and address. Use that database's original password. Do not initialize over it. |
| Old project appears | Use a separate browser profile for a second local board. Localhost cookies can be shared across ports. |
| Database locked | Stop the process that owns it. Keep the database. Do not stop unrelated processes. |
| Import rejected | Compare the CSV with the downloaded descriptors. Check behavior and policy revisions. Keep rejected bytes for diagnosis. |
| Server error | Preserve the database and log. Record the action and time. Check Operator health. Report a defect with synthetic data and redacted logs. |
| Recovery needed | Follow [restore procedures](operators.md). Restore to a new path. Keep the original database. |
