# RepoWorker

RepoWorker is a local Model Context Protocol (MCP) server. It exposes closed-
world repository status, confined repository read/search/patch tools, and
persistent development task handoff over stdio.

## Local development

Requirements: Go 1.26.6, Git, and Lima (only for validating the supplied VM
template).

```sh
make verify
```

The command runs formatting, static analysis, normal and race-detector tests,
the MCP integration tests, `go mod verify`, a reproducible local build, and
Lima template validation. The executable is written to `bin/repoworker`.

To run the server directly, explicitly configure the repository root:

```sh
go run ./cmd/repoworker -repo-root /absolute/path/to/repository
```

Persistent task state defaults to the per-user configuration directory outside
the repository. An explicit absolute state directory may be supplied when
needed:

```sh
go run ./cmd/repoworker \
  -repo-root /absolute/path/to/repository \
  -state-dir /absolute/path/to/private/state
```

The same `-repo-root` argument must be present when another process such as
`tunnel-client` launches `bin/repoworker`; the binary intentionally has no
implicit repository root. If `-state-dir` is supplied it must also remain
outside the configured repository.

### Local tunnel operations

The configured local tunnel uses the `local-stdio` profile at
`~/.config/tunnel-client/local-stdio.yaml`. The profile launches:

```text
/Users/tienphat/Developer/m3-repoworker/bin/repoworker \
  -repo-root /Users/tienphat/Developer/m3-repoworker \
  -state-dir /Users/tienphat/.repoworker-state
```

The restart scripts supply the explicit `-state-dir` override so stale state
does not prevent MCP startup.

Build and verify the binary before starting or restarting the tunnel:

```sh
make verify
```

The credential is supplied through `CONTROL_PLANE_API_KEY`; do not print or
commit its value. The credential-safe restart wrapper loads `.env` privately,
stops the old listener, and starts a single tunnel on `127.0.0.1:8080`:

```sh
./scripts/restart-local-tunnel.sh
```

For a one-command local reset, use the reset wrapper. It verifies and rebuilds
the binary, moves the selected state directory to a timestamped backup, starts
with a fresh state directory outside the repository, and waits for tunnel
readiness:

```sh
./scripts/reset-local.sh
```

Set `REPOWORKER_STATE_DIR` to choose a different private state location. The
old state is moved aside and is not deleted.

The equivalent direct start command, when `CONTROL_PLANE_API_KEY` is already
exported in the shell, is:

```sh
tunnel-client run --profile local-stdio
```

Check configuration and runtime health without exposing credentials:

```sh
tunnel-client doctor --profile local-stdio --explain
lsof -nP -iTCP:8080 -sTCP:LISTEN
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
tail -f /private/tmp/m3-tunnel.log
```

Confirm the local binary still advertises all 12 RepoWorker tools:

```sh
go test ./cmd/repoworker -run TestRepoStatusTool -count=1
```

If ChatGPT shows an older 7-tool schema while the local test and tunnel logs
are correct, reconnect or remove and re-add the MCP connector, then open a new
ChatGPT session. This refreshes the external connector registration; changing
RepoWorker is not a workaround for a cached connector schema.

The server uses newline-delimited JSON on standard input and output. Do not
write human-readable logs to standard output: that stream is reserved for MCP
messages.

## Main-only authority

RepoWorker enforces a main-only development policy independently of client
prompts or agent guidance. The configured checkout must be on `main` at
startup, and every mutating tool re-checks the trusted Git branch before it
runs. `task_create` and `task_resume` bind only to `main`; a non-main checkout
fails closed with `repository must be on main`. RepoWorker exposes no branch or
worktree creation/switching tools, and clients cannot override this policy.

## Security boundaries

- The MCP server uses stdio only; it does not open a network listener.
- A repository root is mandatory, must be an existing absolute directory, and
  is canonicalized before use. Repo paths are always relative to that root.
  Absolute paths, `..` traversal, Windows volume paths, backslashes, and
  symlink escapes are rejected.
- The repository root is also opened once at startup and assigned an opaque
  filesystem identity derived from its device/inode identity. New task state is
  bound to that identity. Repository read, search, and patch operations resolve
  from the opened root with FD-relative operations, `O_NOFOLLOW`, and device-
  crossing checks rather than reopening the startup pathname.
- `repo_read` and `repo_search` only handle bounded UTF-8 regular files. They
  do not follow symlinks while searching and never expose `.git`, `.env`,
  `.env.*`, common credential/secret/token stores, private keys, or common key
  stores. Source-code files such as `token.go` remain accessible so security
  and authentication code can still be edited.
- Repository search is bounded by query size, per-file size, match count,
  scanned file count, scanned bytes, MCP output size, and per-match preview
  size. Search also honors MCP request cancellation. A truncated result is
  reported with `truncated: true`.
- `apply_patch` accepts exactly one existing, non-symlink UTF-8 file and one
  strict unified diff (`--- a/path`, `+++ b/path`). Every hunk must match
  exactly, later and multi-hunk offsets are validated against the original
  file, and the replacement is atomic. File mutations are serialized so two
  concurrent patches cannot both commit from the same stale snapshot.
- `apply_patch` rejects file creation, deletion, renames, multi-file patches,
  and protected paths.
- `task_create` generates the task identifier itself, binds the task to a hash
  of the canonical repository root plus `main` and the current commit, and
  initializes verification state to `RED`.
- `task_status` reads only persisted state. `task_resume` refuses a repository
  mismatch or any checkout that is not on `main`; if the `main` HEAD moved, it
  updates `current_head_sha` and forces the task back to `RED`.
- Task state is written outside the repository with private directory/file
  permissions, atomic replace, file and directory sync, bounded strict JSON
  decoding, and repository-identity checks. Corrupt or mismatched state fails
  closed.
- Git use is limited to fixed local metadata reads (`rev-parse` and
  `symbolic-ref`) with no shell, hooks, prompts, network operations, or arbitrary
  command input. RepoWorker still has no GitHub credentials and cannot publish.
- Rejected MCP requests return only `request rejected`, except for the safe
  policy message `repository must be on main`; they never return a filesystem
  path, repository root, protected filename, task-state location, or underlying
  host error.
- `repo_status`, `repo_read`, `repo_search`, and `task_status` are read-only,
  idempotent, non-destructive, and closed-world. `apply_patch` is explicitly
  destructive. `task_create` and `task_resume` mutate only RepoWorker-owned
  handoff state and are non-destructive to repository contents.
- `repoworker-prod.yaml` preserves the hardened Lima isolation baseline: no
  host mounts, no configured guest networks, SSH over VSOCK disabled, and an
  ignored forwarding rule covering all guest application ports. Do not relax
  these settings without an explicit security review.
- Keep local credentials only in ignored `.env` or `.env.local` files. Never
  add real credentials to source, tests, docs, generated artifacts, or task
  state.

## Task handoff state

A task persists these development fields across RepoWorker restarts:

- `task_id`
- `repo_root_identity`
- `repo_filesystem_identity`
- `branch`
- `base_sha`
- `current_head_sha`
- `last_verified_sha`
- `verification_state` (`RED` or `GREEN`)
- `failed_checks`
- `next_action`

`last_verified_sha` and `GREEN` are reserved for the verification layer. This
phase never manufactures a GREEN state.

## Current scope

RepoWorker currently implements repository status, confined read/search,
deterministic FD-relative snapshot manifests, strict patch application, and
persistent task create/status/resume handoff.
Verification execution, Gatekeeper authority, candidate binding, and publishing
are intentionally not implemented yet. A green local `make verify` is a
development check, not final publish authorization.
