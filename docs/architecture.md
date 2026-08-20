# Architecture

RepoWorker has a live repository authority, an external durable state root,
and isolated task-workspace generations. The live repository is never mounted
as a task workspace. A generation is materialized from a source snapshot,
bound to the source filesystem identity and candidate digest, and guarded by a
lease fence before runtime or integration mutation.

The current M3.1 substrate consists of:

- `internal/taskstate`: production SQLite task state and strict legacy JSON
  migration.
- `internal/workspace`: generation materialization, leases, runtime ownership,
  digest plans, FD-relative integration and crash recovery.
- `internal/memory`: immutable SQLite entries with bounded FTS5 search.
- `internal/security`: typed deny-by-default capabilities, enrollment,
  principal/session binding, replay protection, confirmation classes, runtime
  policy compilation, and redacted audit events.
- `internal/process`: typed supervised processes, bounded cursor streams,
  durable spill, cancellation/timeout cleanup, signals, and optional PTY.
- `internal/runtime`: lifecycle manager with Apple container primary adapter,
  Lima fallback/test adapter, isolated workspace mount binding, and crash
  cleanup.
- `internal/scheduler`: bounded DAG scheduler with CPU/RAM admission, weighted
  fairness, quotas, backpressure, cancellation, and host-pressure response.
- `internal/environment`: deterministic toolchain/lockfile identities,
  environment generations, registry-only installer boundary, and verified
  non-authoritative cache.
- `internal/intelligence`: native ecosystem detection, typed targeted/full
  verification plans, and candidate/environment/policy-bound results.
- `internal/events`: durable SQLite runs, ordered append-only events,
  cursor-resumable logs, digest-verified artifacts, immutable checkpoints,
  retention GC, and invalidation hints.
- `internal/loop`: fixed typed autonomous coding loop with persisted phase
  transitions, bounded retries, failure taxonomy, binding checks, and human
  checkpoints for ambiguous or destructive plans.
- `internal/publication`: dry-run-first Git/jj, GitHub CLI, release, Dagger,
  and Dagu adapters with candidate revalidation, least-privilege gates,
  remote ref checks, and explicit external confirmation.

M3.2–M3.10 will bind authenticated principals, capabilities, process/runtime
backends, resource admission, verification, events, autonomous-loop state,
and publication adapters to these foundations.
