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
capabilities, and one-time confirmation classes. The signed credential nonce
identifies a unique credential; same-session MCP request replay is separately
checked from SDK request metadata and a bounded cache.

Workspace generations reject symlinks and live/cache overlap. Integration
paths are repository-relative and use descriptor-relative no-follow writes,
atomic replacement, fsync, and lease fencing. Candidate snapshots prevent
TOCTOU integration and verification races.

Production MCP is closed-world and has exactly 31 tools. It exposes typed
repository reads plus read-only `repo_git_status`, workspace lifecycle, Apple
runtime lifecycle, supervised process argv, verification, durable runs/events,
autonomous loops, and publication plan/execute. It does not expose
`confirmation_issue`, host execution, arbitrary file creation/deletion, branch
switching, or worktree creation. `process_run` does expose a strong generic
shell capability, but only as `sh/bash/zsh -lc` inside the isolated Apple
runtime with `/workspace` CWD and the candidate-only mount. Public output
omits host paths and uses the stable `/workspace` mount.

Destructive integration/publication confirmation is issued only by an
operator-only authority implemented by the private Unix socket and dedicated
`operator-approve` CLI. Its `0700` state directory, `0600` key/socket, and
HMAC-authenticated operator identity are independent from the autonomous MCP
principal. The record is bound to exact action, repository, principal, session,
generation fence, candidate snapshot, plan digest, expiry, and one-time atomic
consumption. Autonomous MCP callers can request a plan but cannot reach or
invoke the approval authority.

The typed Git status operation runs fixed read-only Git arguments with a
64-KiB stdout ceiling, 256 returned path ceiling, and 16-KiB returned path
summary ceiling. It sorts paths before truncation and fails closed on malformed
or oversized output; it never writes the index or worktree.

`repo_verify` is also a fixed, idempotent read-only operation from the MCP
policy perspective: its allow-listed verification runs use bounded output and
do not alter the repository authority. The true state-changing MCP operations
are guarded by request-level replay protection.

Apple runtime defaults to `--network none`, mounts only the isolated workspace,
applies CPU/RAM limits, and recovers active records before new mutation.
Registry/full network modes fail closed because there is no domain-filtered
Apple network adapter yet. Process execution rejects credential-bearing and
baseline-overriding environment variables, uses a typed executable name and
bounded argv, passes only explicit `key=value` values, and kills the process
group on cancellation/timeout. The baseline PATH includes candidate-local
tool directories but never inherits the host environment.

Event and loop payloads are bounded and reject common credential markers.
Verification diagnostics are redacted and bounded. Publication never places
credentials in command arguments; external mutation is disabled unless the
short-lived gate, fresh binding, remote/ref check, and confirmation all pass.
