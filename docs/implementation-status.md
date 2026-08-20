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
  surface with no autonomous confirmation minting; bounded Git output includes
  deterministic changed-path truncation and no index/worktree mutation;
- a real private operator Unix socket and `operator-approve` CLI with private
  `0700`/`0600` state permissions, independent HMAC operator authentication,
  pending-plan binding, atomic one-time consumption, and restart invalidation;
- MCP receiving-boundary replay protection using SDK `_meta` request identity,
  SDK session handle, authenticated transport/session/principal binding, and a
  bounded cache. Signed HTTP credential nonces are documented as uniqueness
  markers rather than consumed transport replay state.

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
The operator-channel tests cover production pending approval, self-approval,
every binding dimension, expiry, signed-wire replay, concurrent consume, and
session invalidation after reopen. The MCP-level test replays one logical
mutating request and observes no duplicate workspace side effect.

## Known limitations

- The default publication adapter is disabled; external GitHub/release/Dagger/
  Dagu execution still needs an explicitly configured short-lived gate and
  credentials supplied by the operator outside RepoWorker state.
- Environment installation is registry-bound but no production installer is
  enabled in the composition root; verification images must already contain
  the required toolchain.
- The local tunnel is an external connector process. If it caches an older MCP
  schema, reconnect/re-add the connector and open a new session. The final
  fresh session used for this checkpoint exposed exactly the 31 tools listed in
  README, including `repo_git_status`, and no `confirmation_issue`.

## Current source checkpoint

The verified implementation checkpoint is local commit
`26ab3aa45ebca6eee6f5c5596f670464b85a6b98` on `main`, descended from the
prior local source checkpoint `02a44737392bf7b9d6e1b90de6da3e9439fbfa3f`.
No remote push, PR, release, or deployment is part of this work.
