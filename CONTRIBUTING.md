# Contribute a change

1. Open an issue. State the observed result, expected result, and affected requirement.
2. Agree on a ticket and its dependencies before editing.
3. For a behavior defect, add a meaningful test that fails on the unchanged app.
4. Make the smallest fix. Use real storage at the agreed test boundaries.
5. Run affected tests and directly related mutants.
6. Obtain separate Standards and Spec reviews. Assessment, import, and offline-report changes also need the recorded critical review method.
7. Update affected instructions and verification records.

Use Go 1.27.4. Run `go test -count=1 ./...` and `go vet ./...` from the root. On Linux, run `bash .github/scripts/test-linux-storage.sh` for the tagged storage-fault tests. The script needs permission to mount and unmount a private 16 MiB filesystem with `sudo`. CI runs ordinary tests and vet on Windows and Ubuntu. It can be started manually or by a pull request.

Production requirements remain **100% line coverage, 100% branch coverage, and zero exclusions** on Windows and Linux. Go statement coverage is supplemental. It does not establish the required line and branch measurements. The release evidence archive contains the qualified measurement tools and recorded results. Original workstation paths are historical bindings, not portable setup commands.

Keep original failures and missing observations. Never backdate a ticket or test. A new test does not prove an earlier TDD step. A changed measurement method needs owner approval and fresh qualification.

The full mutation campaign stays paused. The M1 exception retains targeted evidence and caps additional targeted checks at 30 minutes total. A new full campaign needs separate authorization.

Use ASD-STE100 Simplified Technical English for public instructions. Use one action per step and consistent [project terms](GLOSSARY.md). Keep interface labels, commands, and identifiers exact.

Do not commit databases, credentials, API keys, private logs, or real evidence. A public source snapshot and a release binary have separate identities.

No project license has been selected. Contact the repository owner about reuse terms. Dependency licenses are supplied separately.
