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

## M3.3 — GREEN

- State: typed supervised process layer implemented and checkpointed on local
  `main`.
- Commit: `0ef2f57` (`m3.3: supervised process layer`).
- Scope: `ProcessSpec`, starter abstraction, monotonic stdout/stderr/PTY
  cursors, bounded memory with durable spill, cancellation/timeout and
  process-group cleanup, signals, environment rejection, and optional PTY.
- Tests: process unit tests cover cursor resume, spill permissions, timeout,
  cancellation, signal cleanup, PTY gating, and credential environment denial.
- Decisions: see `docs/adr/0003-m3.3-supervised-processes.md`.
- Remaining: M3.4 runtime backend abstraction with Apple container primary,
  Lima fallback/test adapter, lifecycle state, isolated workspace mounts,
  network/resource defaults, identity binding, and crash cleanup.
- Exact next action: define the `RuntimeBackend` contract and lifecycle state
  machine, then add Apple container/Lima adapters with mount and network tests.

## M3.4 — GREEN

- State: runtime lifecycle and adapters implemented and checkpointed on local
  `main`.
- Commit: `53fbdd5` (`m3.4: isolated Apple container runtime`).
- Scope: typed lifecycle states, one runtime per generation, Apple container
  primary CLI adapter, Lima fallback/test adapter, isolated mount binding,
  default no-network, CPU/RAM limits, identity binding, persisted lifecycle,
  and crash cleanup/quarantine.
- Tests: runtime tests cover lifecycle, duplicate admission, live mount
  overlap, stale lease, persisted crash recovery, and Apple command binding.
- Decisions: see `docs/adr/0004-m3.4-isolated-runtime.md`.
- Remaining: M3.5 resource-aware parallel execution with CPU/RAM admission,
  weighted fairness, dependency DAG, cancellation/backpressure, quotas, and
  host-pressure response.
- Exact next action: define deterministic resource admission and scheduler
  contracts, then add stress/race/DAG tests.

## M3.5 — GREEN

- State: resource-aware scheduler implemented and checkpointed on local
  `main`.
- Commit: `56a6441` (`m3.5: resource-aware scheduler`).
- Scope: CPU/RAM admission, weighted fair ready selection, dependency DAG,
  bounded submission/backpressure, task/runtime quotas, controlled
  concurrency, errgroup-equivalent cancellation propagation, and host
  pressure response.
- Tests: scheduler tests cover DAG ordering, resource limits, cycle rejection,
  backpressure, sibling cancellation, host pressure, unschedulable jobs, and
  submitted queue execution.
- Decisions: see `docs/adr/0005-m3.5-resource-aware-scheduler.md`.
- Remaining: M3.6 reproducible environment generations, toolchain detection,
  lockfile identities, runtime package installation, registry-only network
  phases, and cache poisoning/cold-cache equivalence protection.
- Exact next action: define environment and cache identities bound to
  toolchain/platform/lockfile/policy, then add cold/warm/corrupt-cache tests.

## M3.6 — GREEN

- State: reproducible environment and cache layer implemented and checkpointed
  on local `main`.
- Commit: `9165206` (`m3.6: reproducible environments and cache`).
- Scope: deterministic Go/Node/Rust/Nx/Turbo/Bazel manifest detection,
  lockfile hashing, environment generations, registry-only installer boundary,
  cache provenance/content verification, poisoning rejection, and cold-cache
  correctness path.
- Tests: environment tests cover deterministic detection/hash, registry-only
  install, cold/warm equivalence, policy-bound cache keys, corruption, cache
  deletion, and symlink manifests.
- Decisions: see `docs/adr/0006-m3.6-reproducible-environments.md`.
- Remaining: M3.7 deterministic repository intelligence adapters and
  candidate/environment/policy-bound verification.
- Exact next action: implement native Go/Node/Rust/Nx/Turbo/Bazel detection,
  targeted/full verification plans, and invalidate results on snapshot change.

## M3.7 — GREEN

- State: repository intelligence and bound verification implemented and
  checkpointed on local `main`.
- Commit: `8b7f704` (`m3.7: repository intelligence and verification`).
- Scope: native ecosystem detection, package-manager selection, targeted/
  affected/full command plans, candidate snapshot checks before/after
  execution, environment/policy binding, and stale-result invalidation.
- Tests: intelligence fixtures cover all six supported ecosystems, native
  command selection, target validation, failure redaction, and TOCTOU snapshot
  changes.
- Decisions: see `docs/adr/0007-m3.7-bound-verification.md`.
- Remaining: M3.8 durable runs/events/artifacts/checkpoints, cursor-resumable
  logs, retention/GC, FSEvents invalidation hints, and crash/restart replay.
- Exact next action: design the durable event schema and append-only replay
  contract, then add retention and restart tests.

## M3.8 — IN PROGRESS

- State: durable run/event/artifact/checkpoint substrate implemented; awaiting
  verification and local checkpoint on `main`.
- Scope: private SQLite runs, ordered append-only events, cursor-resumable log
  reads, digest-verified private artifacts, immutable checkpoints, terminal-run
  retention GC, and advisory FSEvents-style invalidation hints.
- Tests: focused tests cover restart replay, missing-run rejection, cursor
  ordering, artifact corruption, retention cleanup, payload rejection, and
  invalidation-hint consumption.
- Decisions: see `docs/adr/0008-m3.8-durable-events.md`.
- Remaining: run the full repository verification gate, then implement M3.9
  autonomous-loop state and bounded continuation/recovery.
- Exact next action: run `make verify` and `govulncheck`, checkpoint M3.8, and
  begin the durable autonomous-loop controller.
