# Security reports and safe operation

Do not post passwords, private databases, or real evidence in public issues.

Use **Security > Report a vulnerability** if private reporting is available. If it is absent, open an issue asking for a private reporting channel. Do not include exploit details or private files in that request.

Use [Issues](https://github.com/jpet121/Release-Evidence-Board/issues) for an ordinary defect. Include the release tag, binary checksum, operating system, action, time, and redacted error. Use synthetic reproduction data.

This M1 candidate is for synthetic evaluation. Keep the default loopback listener. Protect data, backup, and password files with operating-system permissions. Use private HTTPS for access outside loopback. Assign only the required roles.

Only one process can own a database. Keep verified backups with their checksums. Follow [operator procedures](docs/operators.md) before restore or purge. SUFFICIENT does not authorize deployment.

No support lifetime or response-time agreement is promised. Fixes need source identity, focused verification, review, and updated checksums.
