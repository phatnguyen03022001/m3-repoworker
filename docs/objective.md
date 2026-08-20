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
- The default `runtime_create` image is the locally provisioned
  `repoworker-dev:local` development toolchain; it provides repository-local
  Git/Go/Python/Node/Rust/build tooling without providing host execution.
- MCP exposes typed operations only. A generic shell is permitted only inside
  the isolated Apple TaskWorkspace runtime at `/workspace`; RepoWorker does
  not expose an unrestricted macOS host shell or host path mutation.
- State is private and durable outside the checkout; recovery is fail-closed.
- Publication is plan-first and execute requires fresh verification, rechecks,
  and scoped confirmation.
- Secrets are rejected from environment and durable event/output boundaries.
- Authentication is transport-bound and fail-closed; operator confirmation is
  outside the autonomous MCP surface.
- Development network defaults to none. Registry/full modes fail closed until
  an Apple domain-filtered adapter exists; broad host credentials are never
  forwarded into a runtime.

## Definition of done

- Production composition root wires all subsystem packages and runs recovery
  before mutation.
- Real Apple lifecycle evidence covers create/start/exec/mutation isolation,
  resource/network constraints, stale lease, and crash recovery.
- Real E2E evidence covers workspace → runtime → process → verification → loop
  → publication plan → authorized integration.
- Cold-cache bootstrap followed by offline verification passes in a private
  cache root.
- The exact production MCP surface is closed-world and includes typed
  `repo_git_status` without `confirmation_issue`.
- Operator confirmation uses an authenticated private socket/CLI outside MCP;
  mutating MCP request identity is replay-protected at the SDK receiving
  boundary; Git status output is bounded and deterministic.
- README and operational docs describe the actual commands and limitations.
- `process_run` can execute repository-local shell, lint, build, test, codegen,
  and package-manager commands in the candidate runtime with bounded output,
  timeout, cancellation, and fresh fencing.
