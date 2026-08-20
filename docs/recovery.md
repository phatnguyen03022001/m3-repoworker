# Recovery

Startup checks SQLite integrity/foreign keys and replays workspace integration
journals before mutation. Ambiguous live-file digests quarantine the journal
and fail closed. Legacy JSON migration is strict and one-time.

The runtime manager loads persisted lifecycle records and invokes backend
recovery before the control plane accepts mutation. Active Apple containers are
stopped/deleted or quarantined if cleanup fails. A stale workspace lease cannot
start a runtime, run a process, refresh a candidate, or integrate.

Durable runs contain task, generation, candidate, environment, policy, and
status bindings. Loop state is replayed from bounded event pages. Resume reads
the persisted loop configuration, replans against the current candidate, and
refuses a changed snapshot/environment. Candidate mutation refreshes the
generation snapshot and durable run binding before verification continues.

Verification results are invalidated by any candidate/environment/policy
mismatch. Publication revalidates the candidate immediately before and after a
mutation; Git push also checks the expected remote ref. External adapters
remain disabled by default and every execute request needs scoped confirmation.
