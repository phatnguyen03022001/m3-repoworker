# Security model

The control plane canonicalizes the repository and binds it to opaque
repository/filesystem identities. State, SQLite WAL/SHM, spill, and runtime
records are outside the checkout with private permissions. Requests use an
authenticated local-dev session with nonce replay protection, trusted-main
binding, typed capabilities, and one-time confirmation classes.

Workspace generations reject symlinks and live/cache overlap. Integration
paths are repository-relative and use descriptor-relative no-follow writes,
atomic replacement, fsync, and lease fencing. Candidate snapshots prevent
TOCTOU integration and verification races.

Production MCP is closed-world. It exposes typed repository reads, workspace
lifecycle, Apple runtime lifecycle, supervised process argv, verification,
durable runs/events, autonomous loops, publication plan/execute, and
confirmation. It does not expose shell, host execution, arbitrary file
creation/deletion, branch switching, or worktree creation. Public output omits
host paths and uses the stable /workspace mount.

Apple runtime defaults to --network none, mounts only the isolated workspace,
applies CPU/RAM limits, and recovers active records before new mutation.
Process execution rejects credential-bearing environment variables, uses an
allow-listed executable name and bounded argv, and kills the process group on
cancellation/timeout.

Event and loop payloads are bounded and reject common credential markers.
Verification diagnostics are redacted and bounded. Publication never places
credentials in command arguments; external mutation is disabled unless the
short-lived gate, fresh binding, remote/ref check, and confirmation all pass.
