# Recovery

Startup checks SQLite integrity/foreign keys and replays workspace integration
journals before mutation. The workspace authority holds an exclusive state
lock, validates every persisted generation against repository and filesystem
identity, and quarantines malformed, missing, or ambiguous records. Valid old
leases and runtime-owner records are removed only after runtime recovery so a
new owner must acquire a fresh fence. Legacy JSON migration is strict and
one-time.

The runtime manager loads persisted lifecycle records and invokes backend
recovery before the control plane accepts mutation. Active Apple containers are
stopped/deleted or quarantined if cleanup fails; an old process/runtime is never
resurrected as running. A stale workspace lease cannot start a runtime, run a
process, refresh a candidate, or integrate. When a durable loop references a
stopped runtime after reopen, `ResumeLoop` rehydrates the workspace, acquires a
new lease, creates and starts a new runtime record, refreshes the environment
and verification binding, and appends the new runtime binding before launch.
Failure to recreate or rebind leaves the run rejected; it never manufactures a
GREEN verification result.

Durable runs contain task, generation, candidate, environment, policy, and
status bindings. Loop state is replayed from bounded event pages. Resume reads
the persisted loop configuration, replans against the current candidate, and
refuses a changed snapshot/environment. Candidate mutation refreshes the
generation snapshot and durable run binding before verification continues.

Verification results are invalidated by any candidate/environment/policy
mismatch. Publication revalidates the candidate immediately before and after a
mutation; Git push also checks the expected remote ref. External adapters
remain disabled by default and every execute request needs scoped operator
confirmation. The real gate runs the Apple lifecycle and M3 E2E tests as
required tests; missing prerequisites are `NOT RUN` failures, never GREEN
skips.

The operator socket and replay cache are deliberately process-local recovery
boundaries. Closing/reopening creates a new authenticated control-plane session
and a new request replay cache; old confirmation records and old request
identities are not resurrected. The operator must re-plan and approve again
after restart. A replayed request ID is rejected even if it is presented with a
different MCP session, principal, or transport binding.
