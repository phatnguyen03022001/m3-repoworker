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
  -state-dir "$HOME/.repoworker-state"
```

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
`bootstrap` đã tải đủ module. `make verify` chạy format, vet, test thường và
race, typed MCP surface tests, module verification, build reproducible, và
Lima validation. Binary nằm ở `bin/repoworker`.

Apple prerequisite:

```sh
container machine create --name repoworker alpine:3.22
```

Real Apple E2E dùng `REPOWORKER_REAL_E2E=1`; image có thể chỉ định bằng
`REPOWORKER_APPLE_VERIFY_IMAGE`.

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

Production chỉ expose typed tools: `repo_*`, `workspace_*`, `runtime_*`,
`process_*`, `verification_*`, `run_*`, `loop_*`, `publication_plan`,
`publication_execute`, và `confirmation_issue`. Không có client shell, host
execution, arbitrary file mutation, branch switching, hay worktree creation.
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
