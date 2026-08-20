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

Keep state outside the live checkout. Never reset or overwrite the live
repository to recover a task. If startup rejects identity, integrity, or
recovery, preserve the state directory and inspect it. Local commits are on
main only and are never pushed automatically.
