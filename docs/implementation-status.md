# Implementation status

## M3.1 — GREEN

- State: implementation complete and checkpointed on local `main`.
- Commit: `d54a787` (`m3.1: transactional workspace and state`).
- Scope: SQLite task state with strict JSON migration, private WAL/SHM,
  integrity checks, isolated workspace generations, APFS clone/copy fallback,
  lease/runtime fencing, digest-bound FD-relative integration journals and
  immutable FTS5 memory.
- Tests: `make ci`, `make verify`, and
  `GOCACHE=.cache/go-build govulncheck ./...` are green; package unit/race
  tests cover `internal/taskstate`, `internal/workspace`, and
  `internal/memory`.
- Decisions: see `docs/adr/0001-m3.1-transactional-workspace.md`.
- Remaining: M3.2 typed deny-by-default execution security policy,
  principal/session binding, enrollment, credential/network policy, audit, and
  confirmation classes.
- Exact next action: inspect the current security boundary and implement the
  M3.2 policy compiler and its adversarial tests on local `main`.

## M3.2 — GREEN

- State: typed security authority implemented and checkpointed on local
  `main`.
- Commit: `7243fa9` (`m3.2: execution security policy`).
- Scope: deny-by-default capabilities, repository enrollment and trusted
  integration references, principal/session binding, nonce replay protection,
  confirmation classes, mount/network/execution compilation, credential
  references, and redacted audit events.
- Tests: adversarial unit tests cover deny-by-default, replay, TOCTOU-bound
  enrollment, live/workspace overlap, full-network and host-shell rejection,
  confirmation reuse, and redaction.
- Decisions: see `docs/adr/0002-m3.2-typed-execution-security.md`.
- Remaining: M3.3 typed supervised process layer with bounded output,
  cancellation, timeout, process-group cleanup, cursors, spill, signals, and
  optional PTY.
- Exact next action: define `ProcessSpec` and the supervised process contract,
  bind it to the M3.2 runtime policy, and add leak/race/timeout tests.

## M3.3 — IN PROGRESS

- State: typed supervised process layer implemented locally; verification and
  checkpoint pending.
- Scope: `ProcessSpec`, starter abstraction, monotonic stdout/stderr/PTY
  cursors, bounded memory with durable spill, cancellation/timeout and
  process-group cleanup, signals, environment rejection, and optional PTY.
- Tests: process unit tests cover cursor resume, spill permissions, timeout,
  cancellation, signal cleanup, PTY gating, and credential environment denial.
- Decisions: see `docs/adr/0003-m3.3-supervised-processes.md`.
- Remaining: run full verification and checkpoint on `main`.
- Exact next action: run `make verify`, `govulncheck ./...`, inspect the diff,
  and commit `m3.3: supervised process layer`.
