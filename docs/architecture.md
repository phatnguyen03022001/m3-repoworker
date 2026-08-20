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

M3.2–M3.10 will bind authenticated principals, capabilities, process/runtime
backends, resource admission, verification, events, autonomous-loop state,
and publication adapters to these foundations.
