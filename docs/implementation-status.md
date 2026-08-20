# Implementation status

## M3.1–M3.10 — audit repair on local main

The production path is wired through internal/controlplane and the production
MCP adapter. It includes:

- transactional task state, isolated TaskWorkspace generations, leases,
  descriptor-relative integration, memory, and crash recovery;
- deny-by-default security with transport-authenticated principal/session
  binding, nonce replay protection, trusted-main binding, typed capabilities,
  operator-only confirmations, mount/network/executable policy, and audit;
- bounded supervised processes, Apple container lifecycle, Lima validation,
  scheduler/resource admission, environment identities and private caches;
- native repository intelligence, candidate/environment/policy-bound
  verification, durable runs/events/checkpoints, and bounded autonomous-loop
  resume;
- typed plan-first publication with candidate/ref rechecks and scoped execute
  confirmation;
- durable reopen recovery with generation validation/quarantine, runtime
  stop/delete recovery, fresh lease fencing, deterministic environment
  rehydration, and runtime re-provision/rebind;
- typed read-only `repo_git_status` and an exact 31-tool production MCP
  surface with no autonomous confirmation minting.

Production MCP has no generic shell, host_exec, arbitrary patch, file creation,
branch switching, worktree, or autonomous confirmation-issue tools. Public
output omits host state paths.

## Evidence gates

- Fast verification: `make ci`.
- Full verification: `make verify` (includes real Apple and real M3 E2E gates;
  missing prerequisites are failures).
- Offline verification: `make bootstrap` followed by `make offline-verify`.
- Fresh-cache proof: `scripts/cold-cache-verify.sh`.
- Real Apple lifecycle: `go test ./internal/runtime -run
  TestAppleContainerRealLifecycle -count=1 -v` when the container machine is
  available.
- Real M3 E2E: `REPOWORKER_REAL_E2E=1 go test ./internal/controlplane -run
  TestControlPlaneRealM3EndToEnd -count=1 -v`.

The real Apple test covers workspace-only mount, no-network behavior,
resource evidence, supervised execution, stale leases, and crash recovery.
The E2E test covers workspace, runtime, process, bound verification, durable
loop, publication plan, confirmation-gated integration, live-repository
isolation, and a close/reopen loop resume with runtime recreation.

## Known limitations

- The default publication adapter is disabled; external GitHub/release/Dagger/
  Dagu execution still needs an explicitly configured short-lived gate and
  credentials supplied by the operator outside RepoWorker state.
- Environment installation is registry-bound but no production installer is
  enabled in the composition root; verification images must already contain
  the required toolchain.
- The local tunnel is an external connector process. If it caches an older MCP
  schema, reconnect/re-add the connector and open a new session.

## Current source checkpoint

The verified implementation checkpoint is local commit
`e0a996d5f24cbad5c003f4094cb1b77429ea8f4d` on `main`, descended from the
audited source HEAD `72a2c8c7fd18a496aad368aa4d6542bb7af5080f`. No remote push,
PR, release, or deployment is part of this work.
