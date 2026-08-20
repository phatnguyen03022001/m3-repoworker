# Implementation status

## M3.1–M3.10 — implemented on local main

The production path is wired through internal/controlplane and the production
MCP adapter. It includes:

- transactional task state, isolated TaskWorkspace generations, leases,
  descriptor-relative integration, memory, and crash recovery;
- deny-by-default security with local principal/session binding, nonce replay
  protection, confirmations, mount/network/executable policy, and audit;
- bounded supervised processes, Apple container lifecycle, Lima validation,
  scheduler/resource admission, environment identities and private caches;
- native repository intelligence, candidate/environment/policy-bound
  verification, durable runs/events/checkpoints, and bounded autonomous-loop
  resume;
- typed plan-first publication with candidate/ref rechecks and scoped execute
  confirmation.

Production MCP has no generic shell, host_exec, arbitrary patch, file creation,
branch switching, or worktree tools. Public output omits host state paths.

## Evidence gates

- Warm verification: `make verify`.
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
loop, publication plan, confirmation-gated integration, and live-repository
isolation.

## Known limitations

- The default publication adapter is disabled; external GitHub/release/Dagger/
  Dagu execution still needs an explicitly configured short-lived gate and
  credentials supplied by the operator outside RepoWorker state.
- Environment installation is registry-bound but no production installer is
  enabled in the composition root; verification images must already contain
  the required toolchain.
- The local tunnel is an external connector process. If it caches an older MCP
  schema, reconnect/re-add the connector and open a new session.

## Exact next action

Run the final clean-main gate, confirm the tunnel is ready, and commit the
verified implementation locally on main. Do not push the remote.
