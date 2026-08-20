# Product objective

RepoWorker must make the complete M3 path real:

`ChatGPT → authenticated control plane → isolated TaskWorkspace → Apple
container → supervised process → scheduler → bound verification → durable
events/checkpoints → autonomous loop → optional verified publication`.

## Invariants

- The live checkout is on `main`, is never mounted into a task runtime, and is
  mutated only by a fenced integration journal.
- Every candidate result binds repository identity, workspace snapshot,
  environment identity, and policy version.
- Apple containers receive only the candidate workspace, no live repository,
  and no network by default.
- MCP exposes typed operations only; no arbitrary host shell or path mutation.
- State is private and durable outside the checkout; recovery is fail-closed.
- Publication is plan-first and execute requires fresh verification, rechecks,
  and scoped confirmation.
- Secrets are rejected from environment and durable event/output boundaries.

## Definition of done

- Production composition root wires all subsystem packages and runs recovery
  before mutation.
- Real Apple lifecycle evidence covers create/start/exec/mutation isolation,
  resource/network constraints, stale lease, and crash recovery.
- Real E2E evidence covers workspace → runtime → process → verification → loop
  → publication plan → authorized integration.
- Cold-cache bootstrap followed by offline verification passes in a private
  cache root.
- README and operational docs describe the actual commands and limitations.
