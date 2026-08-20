# Security model

The control plane canonicalizes the repository and binds it to opaque
repository/filesystem identities. State, SQLite WAL/SHM, spill, and runtime
records are outside the checkout with private permissions. A transport
authentication boundary supplies the principal: signed HTTP credentials bind
principal, transport session, expiry, and nonce, while the local connector
mode requires an explicit trusted principal configuration. Missing identity,
forged tool identity, transport mismatch, expiry, and replay fail closed.
The resulting session binds principal, authentication context, repository, and
filesystem identity. Requests use nonce rotation, trusted-main binding, typed
capabilities, and one-time confirmation classes.

Workspace generations reject symlinks and live/cache overlap. Integration
paths are repository-relative and use descriptor-relative no-follow writes,
atomic replacement, fsync, and lease fencing. Candidate snapshots prevent
TOCTOU integration and verification races.

Production MCP is closed-world and has exactly 31 tools. It exposes typed
repository reads plus read-only `repo_git_status`, workspace lifecycle, Apple
runtime lifecycle, supervised process argv, verification, durable runs/events,
autonomous loops, and publication plan/execute. It does not expose
`confirmation_issue`, shell, host execution, arbitrary file creation/deletion,
branch switching, or worktree creation. Public output omits host paths and
uses the stable /workspace mount.

Destructive integration/publication confirmation is issued only by an
operator-only authority (private CLI/admin socket with its own authentication),
then bound to exact action, repository, principal, session, generation fence,
candidate snapshot, plan digest, expiry, and one-time consumption. Autonomous
MCP callers can request a plan but cannot mint their own approval.

Apple runtime defaults to --network none, mounts only the isolated workspace,
applies CPU/RAM limits, and recovers active records before new mutation.
Process execution rejects credential-bearing environment variables, uses an
allow-listed executable name and bounded argv, and kills the process group on
cancellation/timeout.

Event and loop payloads are bounded and reject common credential markers.
Verification diagnostics are redacted and bounded. Publication never places
credentials in command arguments; external mutation is disabled unless the
short-lived gate, fresh binding, remote/ref check, and confirmation all pass.
