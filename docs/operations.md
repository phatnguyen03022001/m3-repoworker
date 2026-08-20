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
  -state-dir "$HOME/.repoworker-state"
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
REPOWORKER_REAL_E2E=1 go test ./internal/controlplane \
  -run TestControlPlaneRealM3EndToEnd -count=1 -v
```

Keep state outside the live checkout. Never reset or overwrite the live
repository to recover a task. If startup rejects identity, integrity, or
recovery, preserve the state directory and inspect it. Local commits are on
main only and are never pushed automatically.
