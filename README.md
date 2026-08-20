# RepoWorker

RepoWorker is a local, authenticated MCP control plane for a main-only Go
repository. Production requests use typed workspace, Apple-container runtime,
supervised-process, bound-verification, durable-run, autonomous-loop, and
plan-first publication operations. The live checkout is never mounted into a
task runtime.

## Khởi động nhanh

Từ thư mục `/Users/tienphat/Developer/m3-repoworker`:

```sh
# Reset state local an toàn, build/verify, và restart tunnel
./scripts/reset-local.sh
```

Nếu chỉ muốn chạy process trực tiếp:

```sh
go run ./cmd/repoworker \
  -repo-root /Users/tienphat/Developer/m3-repoworker \
  -state-dir "$HOME/.repoworker-state" \
  -trusted-principal local-cli
```

`-trusted-principal` is explicit configuration for a connector that has
already authenticated the transport. It is not an MCP caller field; omitting
it fails closed. HTTP transports may instead use the signed
`Authorization: Bearer rw1...` credential bound to the transport session.

Nếu tunnel đã được cấu hình:

```sh
./scripts/restart-local-tunnel.sh
```

`reset-local.sh` không xoá state cũ: nó di chuyển state sang thư mục backup có
timestamp. Có thể đổi vị trí state bằng `REPOWORKER_STATE_DIR`.

## Local development

Requirements: Go 1.26.6, Git, Apple `container` CLI plus a running container
machine for real runtime tests, and Lima only for validating the supplied VM
template.

```sh
make bootstrap
make verify
make offline-verify
./scripts/cold-cache-verify.sh
```

Các lệnh Go dùng cache riêng trong `.cache/`; `offline-verify` chỉ chạy sau khi
`bootstrap` đã tải đủ module. `make verify` chạy fast gates, Lima validation,
và cả hai real DoD gates. Nếu Apple prerequisite thiếu, gate báo `NOT RUN` và
trả lỗi; không coi `SKIP` là GREEN. `make offline-verify` chỉ là offline gate
và không thay thế real DoD gate. Binary nằm ở `bin/repoworker`.

Apple prerequisite:

```sh
container machine create --name repoworker alpine:3.22
```

Real Apple lifecycle gate:

```sh
go test ./internal/runtime \
  -run TestAppleContainerRealLifecycle -count=1 -v
```

Real M3 E2E dùng `REPOWORKER_REAL_E2E=1`; image có thể chỉ định bằng
`REPOWORKER_APPLE_VERIFY_IMAGE`:

```sh
REPOWORKER_REAL_E2E=1 go test ./internal/controlplane \
  -run TestControlPlaneRealM3EndToEnd -count=1 -v
```

## Local tunnel

Tunnel dùng profile `local-stdio` tại
`~/.config/tunnel-client/local-stdio.yaml`, repository/state path rõ ràng, và
listen trên `127.0.0.1:8080`. Credential chỉ đọc riêng từ
`CONTROL_PLANE_API_KEY`:

```sh
./scripts/restart-local-tunnel.sh
tunnel-client doctor --profile local-stdio --explain
curl -fsS http://127.0.0.1:8080/readyz
```

Nếu ChatGPT hiển thị schema cũ sau khi local test và tunnel đều đúng, hãy
reconnect hoặc remove/re-add MCP connector rồi mở session mới; connector có
thể đang cache schema cũ.

## Production MCP surface

Production expose đúng 31 typed tools:

```text
repo_status repo_read repo_search repo_snapshot repo_git_status repo_verify
workspace_create workspace_status workspace_discard workspace_integration_plan workspace_integrate
runtime_create runtime_start runtime_stop runtime_status
process_run process_read process_signal process_cancel process_wait
verification_plan verification_run verification_status
run_create run_event_append run_events
loop_start loop_resume loop_status
publication_plan publication_execute
```

`repo_git_status` là read-only và trả repository identity, trusted root
identity, branch, full HEAD SHA, dirty state, và changed paths. Không có
`confirmation_issue`, client shell, host execution, arbitrary file mutation,
branch switching, hay worktree creation. Confirmation được cấp qua
operator-only authority riêng và không bao giờ qua autonomous MCP.
Candidate edit chỉ xảy ra trong TaskWorkspace đã lease và qua autonomous loop
bị giới hạn.

Verification bind repository, candidate snapshot, environment, và policy.
MCP output dùng opaque IDs và `/workspace`; không trả về state path hay spill
file. Publication mặc định là plan-only; execute cần verification còn hiệu
lực, ref recheck nếu có, và one-time confirmation. External adapters mặc định
disabled.

## Main-only và security

Checkout phải ở `main` khi khởi động; mọi mutation re-check trusted branch.
State nằm ngoài checkout với quyền private. Path là relative, bounded,
symlink-safe, không chạm `.git`, credential stores, hay state root. Apple
runtime chỉ mount workspace cô lập, no-network mặc định, và chạy argv
allow-list qua supervised process group.

Rejected MCP request trả về `request rejected`; credential không được ghi vào
environment/event payload hoặc command arguments.

Xem [docs/objective.md](/Users/tienphat/Developer/m3-repoworker/docs/objective.md),
[docs/architecture.md](/Users/tienphat/Developer/m3-repoworker/docs/architecture.md),
[docs/security-model.md](/Users/tienphat/Developer/m3-repoworker/docs/security-model.md),
và [docs/operations.md](/Users/tienphat/Developer/m3-repoworker/docs/operations.md).
