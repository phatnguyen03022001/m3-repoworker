# Operations

The shortest supported local reset is:

```sh
./scripts/reset-local.sh
```

It moves old state to a timestamped backup, runs verification, builds the
binary, restarts the local tunnel, and waits for readiness. To start without
the tunnel:

```sh
go run ./cmd/repoworker \
  -repo-root /Users/tienphat/Developer/m3-repoworker \
  -state-dir "$HOME/.repoworker-state" \
  -trusted-principal local-cli
```

For a configured MCP tunnel:

```sh
./scripts/restart-local-tunnel.sh
curl -fsS http://127.0.0.1:8080/readyz
```

Dependency and offline gates:

```sh
make bootstrap
make verify
make offline-verify
./scripts/cold-cache-verify.sh
```

Real Apple tests require a running machine:

```sh
container machine create --name repoworker alpine:3.22
go test ./internal/runtime \
  -run TestAppleContainerRealLifecycle -count=1 -v
REPOWORKER_REAL_E2E=1 go test ./internal/controlplane \
  -run TestControlPlaneRealM3EndToEnd -count=1 -v
```

`make verify` runs the fast gates, Lima validation, and both real gates. The
real tests return a failure with `NOT RUN` when the Apple prerequisite is
missing; an integration test must not be silently skipped. `offline-verify`
and the cold-cache script intentionally cover only the reproducible offline
gate, followed by the sequential production verification-preset check.

The connector command must carry an explicit trusted principal (the provided
tunnel script uses `local-tunnel`), or a signed authenticated transport header
must be supplied. RepoWorker never accepts identity from an MCP tool argument.

## Operator confirmation channel

The production binary starts a separate operator-only Unix socket at
`<state-dir>/operator.sock` and creates `<state-dir>/operator.key`. The state
directory is `0700`, the key and socket are `0600`, and the key is independent
of the autonomous MCP principal. No MCP tool can issue a confirmation and
`confirmation_issue` is intentionally absent from the production surface.

After `repo_status`, `workspace_status`, and
`workspace_integration_plan` have returned the current repository, principal,
session, generation, fencing, candidate, and plan identities, an operator can
approve the pending integration with the dedicated CLI:

```sh
./bin/repoworker operator-approve \
  -socket "$REPOWORKER_STATE_DIR/operator.sock" \
  -operator-key-file "$REPOWORKER_STATE_DIR/operator.key" \
  -operator-id local-operator \
  -class destructive \
  -operation integrate \
  -repository-id "$REPOSITORY_ID" \
  -principal-id "$PRINCIPAL_ID" \
  -session-id "$SESSION_ID" \
  -generation-id "$GENERATION_ID" \
  -fencing-generation "$FENCING_GENERATION" \
  -candidate-snapshot "$CANDIDATE_SNAPSHOT" \
  -plan-digest "$PLAN_DIGEST"
```

The command prints the opaque one-time token to stdout for the operator to
submit to `workspace_integrate`; it never logs the token. `-operation
integrate` derives the exact action digest from the plan digest. The socket
authenticates the operator with the private HMAC key and binds the confirmation
to action, repository, principal, session, generation, fencing generation,
candidate snapshot, plan digest, class, and expiry. A scope change, restart,
expiry, replay, or concurrent second consume is rejected.

## Development shell and general lint execution

After `workspace_create`, `runtime_create`, and `runtime_start`, use the
existing typed `process_run` tool. It does not invoke a macOS shell directly:
the command is supervised by the Apple container runtime and runs with CWD
under `/workspace`.

```text
executable: sh
arguments: ["-lc", "printf shell-ok && go test ./..."]
cwd: /workspace
timeout_seconds: 120
```

The same path supports `bash -lc` or `zsh -lc` when present in the selected
image, repository-local `make` and `./scripts/*.sh`, Go/Node/Python/Rust
lint/build/test/codegen commands, and package-manager scripts. Examples are
`go test -race ./...`, `go vet ./...`, `npm run lint`, `npx eslint .`,
`python -m pytest`, `ruff check .`, `cargo clippy`, and `make verify`.
Commands, output, timeout, memory, process-group cancellation, runtime state,
workspace generation, and fencing remain bounded by the existing process
subsystem.

The runtime PATH is deterministic and includes `/workspace/node_modules/.bin`,
`/workspace/.venv/bin`, and `/workspace/bin`. Explicit MCP environment values
must be bounded non-secret `NAME=value` entries; host environment, keychain,
brew, sudo, host pip, and host npm-global state are not forwarded. The
candidate workspace may contain local Git operations, but the live checkout is
never mounted and authenticated push remains outside this capability.

Network mode is `none` by default. `registry` and `full` requests are rejected
until an Apple adapter can enforce domain filtering; RepoWorker does not widen
network access as a substitute.

Mutating MCP calls must carry the SDK request metadata keys
`repoworker/request_id` and `repoworker/request_sequence` in
`CallToolParams._meta`. The bounded process-local cache rejects duplicate
request IDs in any bound session and duplicate sequences in one MCP session.
Restart creates a fresh authenticated control-plane session and replay cache;
callers must establish a fresh MCP session and request sequence. The signed
HTTP credential nonce is a credential-uniqueness marker, not a consumed
transport replay cache.

`repo_git_status` is read-only and bounded: it returns deterministic sorted
path summaries, `changed_count`, and `truncated`; oversized or malformed Git
output fails closed.

Keep state outside the live checkout. Never reset or overwrite the live
repository to recover a task. If startup rejects identity, integrity, or
recovery, preserve the state directory and inspect it. Local commits are on
main only and are never pushed automatically.
