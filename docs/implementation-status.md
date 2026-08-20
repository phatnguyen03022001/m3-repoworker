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
- M3 development shell/general lint execution through `process_run`: `sh`,
  `bash`, and `zsh` are allowed only inside the Apple container; controlled
  Python/Node/Rust/Go/tooling executable policy, deterministic candidate-local
  PATH, bounded non-secret environment overrides, no-network default, and a
  locally provisioned `repoworker-dev:local` image with Git/Go/Python/Node/
  Rust/build tooling;
- candidate generations receive fresh isolated Git metadata and a synthetic
  base commit for local `git diff`/`add`/`restore`/`commit`; live `.git` is
  never copied, mounted, or included in candidate snapshots/integration;
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
- MCP receiving-boundary replay protection derives SDK `_meta` request identity
  from the actual JSON-RPC request ID and canonical call payload, then binds it
  to the SDK session handle and authenticated transport/session/principal in a
  bounded cache. Signed HTTP credential nonces are documented as uniqueness
  markers rather than consumed transport replay state.

Production MCP has no host_exec, arbitrary patch, file creation, branch
switching, worktree, or autonomous confirmation-issue tools. It retains the
exact 31-tool surface; `process_run` is the generic development command path,
but generic shell is permitted only inside the isolated TaskWorkspace runtime.
RepoWorker does not expose an unrestricted macOS host shell. Public output
omits host state paths.

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
The E2E test covers workspace, runtime, process, shell `go test ./...`,
failing-shell exit status, candidate-only mutation, process timeout cleanup,
bound verification, durable loop, publication plan, confirmation-gated
integration, live-repository isolation, and a close/reopen loop resume with
runtime recreation.
The operator-channel tests cover production pending approval, self-approval,
every binding dimension, expiry, signed-wire replay, concurrent consume, and
session invalidation after reopen. The MCP-level test replays one logical
mutating request and observes no duplicate workspace side effect.

## Known limitations

- The default publication adapter is disabled; external GitHub/release/Dagger/
  Dagu execution still needs an explicitly configured short-lived gate and
  credentials supplied by the operator outside RepoWorker state.
- Environment installation is registry-bound but no production installer is
  enabled in the composition root; the local default development image is
  provisioned explicitly with `scripts/build-development-image.sh`, while
  registry/full runtime networking remains fail closed until domain filtering
  is implemented. Explicit image overrides must provide their own toolchain.
- The local tunnel is an external connector process. If it caches an older MCP
  schema, reconnect/re-add the connector and open a new session. The tunnel
  has been rebuilt/restarted with the current binary; a fresh frontend
  mutation probe remains part of live evidence because the connector is
  external to this repository.

## Current source checkpoint

The implementation checkpoint is local commit
`47dec4f` (`m3: provide container development toolchain`) on `main`.
Automated, real Apple, real M3 E2E, candidate Git isolation, and local
wire-level replay gates are green. The documentation-only follow-up commit
may advance `HEAD`; verify the exact full current hash with `git rev-parse
HEAD`. The external frontend connector remains an external evidence surface
and must use a fresh session after tunnel restart. No remote push, PR, release,
or deployment is part of this checkpoint.
